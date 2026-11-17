package netd

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"glasshouse/internal/audit"
)

// Gateway lets the guest call the model API without ever holding the real key.
// The guest is configured with ANTHROPIC_BASE_URL=<this listener> and the
// per-session token as its "API key"; the gateway swaps in the real key from
// the host's secret store on the way out.
type keyCtx struct{}

type Gateway struct {
	token    string
	key      func() (string, error)
	audit    audit.Writer
	proxy    *httputil.ReverseProxy
	requests atomic.Int64
	// MaxRequests caps model calls per session; 0 = unlimited.
	MaxRequests int64
}

func NewGateway(upstream, token string, key func() (string, error), aud audit.Writer) (*Gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("gateway: invalid upstream URL")
	}
	g := &Gateway{token: token, key: key, audit: aud}
	g.proxy = &httputil.ReverseProxy{
		FlushInterval: -1, // stream server-sent events immediately
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			// Drop everything the guest supplied that could carry identity.
			for _, h := range []string{"X-Api-Key", "Authorization", "Cookie", "X-Forwarded-For", "Forwarded"} {
				pr.Out.Header.Del(h)
			}
			// The real key replaces the guest's placeholder.
			if k, ok := pr.In.Context().Value(keyCtx{}).(string); ok {
				pr.Out.Header.Set("X-Api-Key", k)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			_ = g.audit.Write("llm.error", map[string]any{"path": r.URL.Path, "error": "upstream request failed"})
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
	return g, nil
}

func (g *Gateway) Requests() int64 { return g.requests.Load() }

func (g *Gateway) authorized(r *http.Request) bool {
	got := r.Header.Get("X-Api-Key")
	if got == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			got = a[len("Bearer "):]
		}
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) == 1
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Only the model API; nothing else on the upstream host is reachable.
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		_ = g.audit.Write("llm.deny", map[string]any{"path": r.URL.Path, "reason": "path-not-allowed"})
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}
	n := g.requests.Add(1)
	if g.MaxRequests > 0 && n > g.MaxRequests {
		_ = g.audit.Write("llm.deny", map[string]any{"path": r.URL.Path, "reason": "request-budget"})
		http.Error(w, "request budget exhausted", http.StatusTooManyRequests)
		return
	}
	key, err := g.key()
	if err != nil || key == "" {
		_ = g.audit.Write("llm.error", map[string]any{"path": r.URL.Path, "error": "no API key available on host"})
		http.Error(w, "gateway has no API key configured", http.StatusBadGateway)
		return
	}
	if err := g.audit.Write("llm.request", map[string]any{"method": r.Method, "path": r.URL.Path, "n": n}); err != nil {
		http.Error(w, "audit failure", http.StatusServiceUnavailable)
		return
	}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyCtx{}, key)))
}

func (g *Gateway) Serve(l net.Listener) error {
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 15 * time.Second}
	return srv.Serve(l)
}

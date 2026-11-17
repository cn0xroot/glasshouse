package netd

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Proxy is an HTTP/HTTPS forward proxy in front of Netd. The guest points
// http_proxy/https_proxy at it. A per-session token (HTTP Basic proxy auth)
// keeps other local processes from using the same listener.
type Proxy struct {
	n         *Netd
	token     string
	transport *http.Transport
}

func NewProxy(n *Netd, token string) *Proxy {
	p := &Proxy{n: n, token: token}
	p.transport = &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DialContext: func(ctx context.Context, _ string, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(portStr)
			return n.Dial(ctx, host, port)
		},
	}
	return p
}

func (p *Proxy) authorized(r *http.Request) bool {
	h := r.Header.Get("Proxy-Authorization")
	const pfx = "Basic "
	if !strings.HasPrefix(h, pfx) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(pfx):])
	if err != nil {
		return false
	}
	_, pass, ok := strings.Cut(string(raw), ":")
	return ok && subtle.ConstantTimeCompare([]byte(pass), []byte(p.token)) == 1
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="glasshouse"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.IsAbs() && r.URL.Scheme == "http" {
		p.forward(w, r)
		return
	}
	http.Error(w, "this is a proxy; send absolute http:// URLs or CONNECT", http.StatusBadRequest)
}

func splitTarget(hostport string, defPort int) (string, int, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			return strings.Trim(hostport, "[]"), defPort, nil
		}
		return "", 0, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func (p *Proxy) denyStatus(err error) int {
	var de *DenyError
	if errors.As(err, &de) {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitTarget(r.Host, 443)
	if err != nil {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	up, err := p.n.Dial(r.Context(), host, port)
	if err != nil {
		w.Header().Set("X-Glasshouse-Deny", err.Error())
		http.Error(w, err.Error(), p.denyStatus(err))
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() { // client -> upstream (this direction counts as egress)
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, up)
		done <- struct{}{}
	}()
	<-done
	// One side finished: tear both down so the other copy returns.
	up.Close()
	client.Close()
	<-done
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		w.Header().Set("X-Glasshouse-Deny", err.Error())
		http.Error(w, err.Error(), p.denyStatus(err))
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Serve runs the proxy on l until it is closed.
func (p *Proxy) Serve(l net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 15 * time.Second}
	return srv.Serve(l)
}

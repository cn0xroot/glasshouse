package netd

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"glasshouse/internal/audit"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	v, ok := f[host]
	if !ok {
		return nil, errors.New("nxdomain")
	}
	var out []netip.Addr
	for _, s := range v {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

type recAudit struct {
	mu   sync.Mutex
	recs []string
	fail bool
}

func (r *recAudit) Write(typ string, f map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("disk full")
	}
	r.recs = append(r.recs, typ)
	return nil
}

func (r *recAudit) has(typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.recs {
		if t == typ {
			return true
		}
	}
	return false
}

var _ audit.Writer = (*recAudit)(nil)

// newTestNetd returns a Netd whose pinned dial is redirected to target and
// records which address it was asked to dial.
func newTestNetd(t *testing.T, target string, allow []string, res fakeResolver, aud audit.Writer, maxEgress int64) (*Netd, *string) {
	t.Helper()
	dialed := new(string)
	n, err := New(Config{
		Allow: allow, MaxEgress: maxEgress, Audit: aud, Resolver: res,
		HostAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("203.0.113.77")} },
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			*dialed = addr
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, dialed
}

func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func TestAllowlistMatch(t *testing.T) {
	al, err := parseAllowlist([]string{"github.com:443", "*.example.com:443", "Pypi.org:443"})
	if err != nil {
		t.Fatal(err)
	}
	yes := [][2]any{{"github.com", 443}, {"GITHUB.COM.", 443}, {"a.example.com", 443}, {"a.b.example.com", 443}, {"pypi.org", 443}}
	no := [][2]any{{"github.com", 80}, {"example.com", 443}, {"evilgithub.com", 443}, {"github.com.evil.com", 443}, {"xexample.com", 443}}
	for _, c := range yes {
		if !al.match(c[0].(string), c[1].(int)) {
			t.Errorf("should allow %v", c)
		}
	}
	for _, c := range no {
		if al.match(c[0].(string), c[1].(int)) {
			t.Errorf("should deny %v", c)
		}
	}
}

func TestBlockedAddr(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("203.0.113.77")}
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "fe80::1", "fc00::1", "198.18.0.1", "240.0.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "203.0.113.77", "2002::1", "64:ff9b::1",
	}
	for _, s := range blocked {
		if b, _ := BlockedAddr(netip.MustParseAddr(s), self); !b {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if b, r := BlockedAddr(netip.MustParseAddr(s), self); b {
			t.Errorf("%s should be allowed (%s)", s, r)
		}
	}
}

func TestDialRules(t *testing.T) {
	echo := echoServer(t)
	aud := &recAudit{}
	res := fakeResolver{
		"good.test":    {"93.184.216.34"},
		"rebind.test":  {"93.184.216.34", "10.0.0.5"},
		"private.test": {"192.168.1.10"},
		"meta.test":    {"169.254.169.254"},
		"self.test":    {"203.0.113.77"},
		"v6map.test":   {"::ffff:127.0.0.1"},
	}
	allow := []string{"good.test:443", "rebind.test:443", "private.test:443", "meta.test:80", "self.test:443", "v6map.test:443"}
	n, dialed := newTestNetd(t, echo, allow, res, aud, 0)

	conn, err := n.Dial(context.Background(), "good.test", 443)
	if err != nil {
		t.Fatalf("good.test: %v", err)
	}
	conn.Close()
	if *dialed != "93.184.216.34:443" {
		t.Fatalf("must dial the pinned IP, dialed %q", *dialed)
	}

	for _, tc := range []struct {
		host   string
		port   int
		reason string
	}{
		{"not-listed.test", 443, "not-in-allowlist"},
		{"good.test", 80, "not-in-allowlist"},
		{"rebind.test", 443, "private-or-host-address"},
		{"private.test", 443, "private-or-host-address"},
		{"meta.test", 80, "private-or-host-address"},
		{"self.test", 443, "private-or-host-address"},
		{"v6map.test", 443, "private-or-host-address"},
		{"127.0.0.1", 443, "ip-literal"},
		{"[::1]", 443, "ip-literal"},
		{"good.test", 0, "bad-target"},
	} {
		_, err := n.Dial(context.Background(), tc.host, tc.port)
		var de *DenyError
		if !errors.As(err, &de) || de.Reason != tc.reason {
			t.Errorf("%s:%d: got %v, want reason %s", tc.host, tc.port, err, tc.reason)
		}
	}
	if !aud.has("net.allow") || !aud.has("net.deny") || !aud.has("net.close") {
		t.Fatalf("audit records missing: %v", aud.recs)
	}
}

func TestFailClosedOnAuditError(t *testing.T) {
	echo := echoServer(t)
	aud := &recAudit{fail: true}
	n, dialed := newTestNetd(t, echo, []string{"good.test:443"}, fakeResolver{"good.test": {"93.184.216.34"}}, aud, 0)
	if _, err := n.Dial(context.Background(), "good.test", 443); err == nil {
		t.Fatal("must refuse when the audit log cannot be written")
	}
	if *dialed != "" {
		t.Fatal("must not have dialed")
	}
}

func TestEgressBudget(t *testing.T) {
	echo := echoServer(t)
	n, _ := newTestNetd(t, echo, []string{"good.test:443"}, fakeResolver{"good.test": {"93.184.216.34"}}, &recAudit{}, 10)
	c, err := n.Dial(context.Background(), "good.test", 443)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write beyond budget must fail")
	}
	if _, err := n.Dial(context.Background(), "good.test", 443); err == nil {
		t.Fatal("new connections must be refused once the budget is spent")
	}
	if n.Egress() != 10 {
		t.Fatalf("egress = %d", n.Egress())
	}
}

func proxyServer(t *testing.T, n *Netd, token string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(n, token)
	go p.Serve(l)
	t.Cleanup(func() { l.Close() })
	return l.Addr().String()
}

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("glasshouse:"+token))
}

func connectVia(t *testing.T, proxy, target, auth string) (*http.Response, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	c.Write([]byte(req + "\r\n"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(c), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return resp, c
}

func TestProxyConnect(t *testing.T) {
	echo := echoServer(t)
	n, _ := newTestNetd(t, echo, []string{"good.test:443"}, fakeResolver{"good.test": {"93.184.216.34"}, "x.test": {"10.0.0.1"}}, &recAudit{}, 0)
	addr := proxyServer(t, n, "s3cret")

	if resp, c := connectVia(t, addr, "good.test:443", ""); resp.StatusCode != 407 {
		t.Errorf("no auth: %d", resp.StatusCode)
	} else {
		c.Close()
	}
	if resp, c := connectVia(t, addr, "good.test:443", basic("wrong")); resp.StatusCode != 407 {
		t.Errorf("bad token: %d", resp.StatusCode)
	} else {
		c.Close()
	}
	if resp, c := connectVia(t, addr, "evil.test:443", basic("s3cret")); resp.StatusCode != 403 {
		t.Errorf("not allowlisted: %d", resp.StatusCode)
	} else {
		c.Close()
	}
	resp, c := connectVia(t, addr, "good.test:443", basic("s3cret"))
	if resp.StatusCode != 200 {
		t.Fatalf("allowed CONNECT: %d", resp.StatusCode)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(bufioReaderFrom(c, resp), buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through tunnel: %q %v", buf, err)
	}
}

func TestProxyPlainHTTP(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked upstream")
		}
		w.Write([]byte("hello " + r.URL.Path))
	}))
	defer origin.Close()
	n, _ := newTestNetd(t, origin.Listener.Addr().String(), []string{"archive.test:80"}, fakeResolver{"archive.test": {"93.184.216.34"}}, &recAudit{}, 0)
	addr := proxyServer(t, n, "tok")

	get := func(host string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", "http://"+host+"/pool/x", nil)
		req.Header.Set("Proxy-Authorization", basic("tok"))
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := req.WriteProxy(c); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufioReader(c), req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := get("archive.test"); resp.StatusCode != 200 || body != "hello /pool/x" {
		t.Fatalf("allowed: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get("blocked.test"); resp.StatusCode != 403 {
		t.Fatalf("blocked: %d", resp.StatusCode)
	}
}

func TestGatewayInjectsKey(t *testing.T) {
	var gotKey, gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuth, gotPath = r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer up.Close()
	aud := &recAudit{}
	g, err := NewGateway(up.URL, "sess-token", func() (string, error) { return "REAL-KEY", nil }, aud)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	do := func(path, key string, hdr map[string]string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader("{}"))
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("/v1/messages", "sess-token", map[string]string{"Authorization": "Bearer guest-secret"}); c != 200 {
		t.Fatalf("status %d", c)
	}
	if gotKey != "REAL-KEY" || gotAuth != "" || gotPath != "/v1/messages" {
		t.Fatalf("upstream saw key=%q auth=%q path=%q", gotKey, gotAuth, gotPath)
	}
	gotPath = ""
	if c := do("/v1/messages", "wrong", nil); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := do("/v1/messages", "", nil); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := do("/admin/keys", "sess-token", nil); c != 403 {
		t.Errorf("non-/v1 path: %d", c)
	}
	if gotPath != "" {
		t.Error("rejected requests must not reach upstream")
	}
	g.MaxRequests = 1
	if c := do("/v1/messages", "sess-token", nil); c != 429 {
		t.Errorf("budget: %d", c)
	}
	if !aud.has("llm.request") || !aud.has("llm.deny") {
		t.Errorf("audit: %v", aud.recs)
	}
}

func TestGatewayNoKey(t *testing.T) {
	g, _ := NewGateway("http://127.0.0.1:1", "t", func() (string, error) { return "", errors.New("none") }, &recAudit{})
	srv := httptest.NewServer(g)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("X-Api-Key", "t")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

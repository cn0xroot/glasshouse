// Package netd is the host-side network gate for the guest. Everything the
// guest sends to the outside world arrives here as an HTTP proxy request (or
// as an LLM gateway request) and is checked against the session policy before
// a single byte leaves the machine.
package netd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"glasshouse/internal/audit"
)

// Resolver is the subset of net.Resolver that netd needs.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type Config struct {
	Allow     []string // "host:port" or "*.suffix:port"
	MaxEgress int64    // bytes the guest may send out; 0 = unlimited
	Audit     audit.Writer
	Resolver  Resolver // default: net.DefaultResolver
	// Dial opens the pinned connection; default: net.Dialer with a timeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// HostAddrs returns the host's own addresses; default: all interface addresses.
	HostAddrs func() []netip.Addr
}

type Netd struct {
	cfg     Config
	allow   allowlist
	egress  atomic.Int64
	ingress atomic.Int64
}

type DenyError struct{ Reason string }

func (e *DenyError) Error() string { return "denied: " + e.Reason }

func New(cfg Config) (*Netd, error) {
	al, err := parseAllowlist(cfg.Allow)
	if err != nil {
		return nil, err
	}
	if cfg.Audit == nil {
		return nil, errors.New("netd: an audit writer is required")
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: 15 * time.Second}
		cfg.Dial = d.DialContext
	}
	if cfg.HostAddrs == nil {
		cfg.HostAddrs = interfaceAddrs
	}
	return &Netd{cfg: cfg, allow: al}, nil
}

func (n *Netd) Egress() int64  { return n.egress.Load() }
func (n *Netd) Ingress() int64 { return n.ingress.Load() }

// Dial applies the full rule chain and returns a connection pinned to the
// resolved address. The caller never gets to dial by name, which is what
// defeats DNS rebinding. Every outcome is audited; if the audit write fails
// the connection is refused (fail closed).
func (n *Netd) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	host = normHost(host)
	addr, err := n.check(ctx, host, port)
	if err != nil {
		var de *DenyError
		reason := "error"
		if errors.As(err, &de) {
			reason = de.Reason
		}
		if aerr := n.cfg.Audit.Write("net.deny", map[string]any{"host": host, "port": port, "reason": reason}); aerr != nil {
			return nil, fmt.Errorf("audit failure: %w", aerr)
		}
		return nil, err
	}
	if aerr := n.cfg.Audit.Write("net.allow", map[string]any{"host": host, "port": port, "addr": addr.String()}); aerr != nil {
		return nil, fmt.Errorf("audit failure: %w", aerr)
	}
	conn, err := n.cfg.Dial(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	if err != nil {
		_ = n.cfg.Audit.Write("net.error", map[string]any{"host": host, "port": port, "error": err.Error()})
		return nil, err
	}
	return &countConn{Conn: conn, n: n, host: host, port: port}, nil
}

// check runs the rules in order and returns the address to dial.
func (n *Netd) check(ctx context.Context, host string, port int) (netip.Addr, error) {
	if host == "" || port < 1 || port > 65535 {
		return netip.Addr{}, &DenyError{"bad-target"}
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return netip.Addr{}, &DenyError{"ip-literal"}
	}
	if !n.allow.match(host, port) {
		return netip.Addr{}, &DenyError{"not-in-allowlist"}
	}
	if n.cfg.MaxEgress > 0 && n.egress.Load() >= n.cfg.MaxEgress {
		return netip.Addr{}, &DenyError{"egress-budget"}
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := n.cfg.Resolver.LookupNetIP(rctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, &DenyError{"dns-failure"}
	}
	self := n.cfg.HostAddrs()
	// Any blocked answer poisons the whole name: a hostname that resolves to
	// both a public and a private address is treated as hostile.
	for _, a := range addrs {
		if blocked, _ := BlockedAddr(a, self); blocked {
			return netip.Addr{}, &DenyError{"private-or-host-address"}
		}
	}
	return addrs[0].Unmap(), nil
}

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	zeroNet   = netip.MustParsePrefix("0.0.0.0/8")
	benchmark = netip.MustParsePrefix("198.18.0.0/15")
	reserved  = netip.MustParsePrefix("240.0.0.0/4")
	ietfProto = netip.MustParsePrefix("192.0.0.0/24")
	v6Doc     = netip.MustParsePrefix("2001:db8::/32")
	v6NAT64   = netip.MustParsePrefix("64:ff9b::/96")
	v6Teredo  = netip.MustParsePrefix("2001::/32")
	v66to4    = netip.MustParsePrefix("2002::/16")
)

// BlockedAddr reports whether the guest must never reach a. self is the list
// of the host's own addresses (including public ones).
func BlockedAddr(a netip.Addr, self []netip.Addr) (bool, string) {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return true, "invalid"
	case a.IsLoopback():
		return true, "loopback"
	case a.IsPrivate():
		return true, "private"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast():
		return true, "link-local" // includes 169.254.169.254 cloud metadata
	case a.IsMulticast():
		return true, "multicast"
	case a.IsUnspecified():
		return true, "unspecified"
	}
	for _, p := range []netip.Prefix{cgnat, zeroNet, benchmark, reserved, ietfProto, v6Doc, v6NAT64, v6Teredo, v66to4} {
		if p.Contains(a) {
			return true, "reserved"
		}
	}
	for _, s := range self {
		if s.Unmap() == a {
			return true, "host-address"
		}
	}
	return false, ""
}

func interfaceAddrs() []netip.Addr {
	var out []netip.Addr
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out
}

type countConn struct {
	net.Conn
	n      *Netd
	host   string
	port   int
	sent   atomic.Int64
	recv   atomic.Int64
	closed atomic.Bool
}

func (c *countConn) Write(p []byte) (int, error) {
	if lim := c.n.cfg.MaxEgress; lim > 0 && c.n.egress.Load() >= lim {
		_ = c.Close()
		return 0, &DenyError{"egress-budget"}
	}
	w, err := c.Conn.Write(p)
	c.n.egress.Add(int64(w))
	c.sent.Add(int64(w))
	return w, err
}

func (c *countConn) Read(p []byte) (int, error) {
	r, err := c.Conn.Read(p)
	c.n.ingress.Add(int64(r))
	c.recv.Add(int64(r))
	return r, err
}

func (c *countConn) Close() error {
	err := c.Conn.Close()
	if c.closed.CompareAndSwap(false, true) {
		_ = c.n.cfg.Audit.Write("net.close", map[string]any{
			"host": c.host, "port": c.port, "sent": c.sent.Load(), "received": c.recv.Load(),
		})
	}
	return err
}

func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

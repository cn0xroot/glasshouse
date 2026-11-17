package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"glasshouse/internal/backend"
	"glasshouse/internal/session"
)

const monitorCookie = "glasshouse_monitor"

// cmdMonitor opens the CC-Monitor web UI that runs inside the guest. That UI
// has no authentication of its own, so it is never exposed directly: the UI is
// reached over an SSH unix-socket forward, and a token-protected loopback proxy
// on the host is the only way in. Requests with a foreign Host header are
// refused, which stops DNS-rebinding attacks from a browser tab.
func cmdMonitor(args []string) error {
	noOpen := false
	var rest []string
	for _, a := range args {
		if a == "--no-open" {
			noOpen = true
		} else {
			rest = append(rest, a)
		}
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	id := ""
	if len(rest) > 0 {
		id = rest[0]
	}
	in, err := loadRunning(home, id)
	if err != nil {
		return err
	}
	if !in.CCMonitor {
		return errors.New("CC-Monitor is disabled for this session (policy monitor.cc_monitor)")
	}
	dir := session.Dir(home, in.ID)
	be, err := backendByName(in.Backend)
	if err != nil {
		return err
	}
	spec := specFromInfo(home, in, be)

	sock := filepath.Join(dir, "monitor.sock")
	_ = os.Remove(sock)
	tunnel, err := startTunnel(be, spec, sock)
	if err != nil {
		return err
	}
	defer func() {
		_ = tunnel.Process.Kill()
		_ = tunnel.Wait()
		_ = os.Remove(sock)
	}()
	if err := waitForFile(sock, 15*time.Second); err != nil {
		return fmt.Errorf("could not reach the CC-Monitor UI in the guest (is glasshouse-cc-monitor running? try: glasshouse ssh -- systemctl status glasshouse-cc-monitor): %w", err)
	}

	token, err := session.NewToken()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	hostPort := l.Addr().String()

	target := &url.URL{Scheme: "http", Host: "cc-monitor.guest"}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	director := rp.Director
	rp.Director = func(r *http.Request) {
		director(r)
		r.Host = "127.0.0.1:9999"
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != hostPort { // DNS-rebinding defence
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: monitorCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
			return
		}
		c, err := r.Cookie(monitorCookie)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) != 1 {
			http.Error(w, "forbidden: open the URL printed by `glasshouse monitor`", http.StatusForbidden)
			return
		}
		cookies := r.Cookies()
		r.Header.Del("Cookie")
		for _, ck := range cookies {
			if ck.Name != monitorCookie {
				r.AddCookie(ck)
			}
		}
		rp.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	go srv.Serve(l)
	defer srv.Close()

	if err := auditEvent(dir, "monitor.open", map[string]any{"listen": hostPort}); err != nil {
		return err
	}
	u := fmt.Sprintf("http://%s/?t=%s", hostPort, token)
	fmt.Println("CC-Monitor UI:", u)
	fmt.Println("press Ctrl-C to close the tunnel")
	if !noOpen {
		openBrowser(u)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-waitCmd(tunnel):
		return fmt.Errorf("tunnel closed: %v", err)
	}
	return nil
}

// startTunnel forwards <sock> (host unix socket) to 127.0.0.1:9999 in the guest.
func startTunnel(be backend.Backend, spec backend.Spec, sock string) (*exec.Cmd, error) {
	argv := be.SSHArgs(spec, "agent", filepath.Join(spec.Dir, "id_ed25519"))
	// SSHArgs ends with "user@host"; insert options before it.
	last := argv[len(argv)-1]
	opts := append([]string{}, argv[1:len(argv)-1]...)
	opts = append(opts, "-N", "-o", "ExitOnForwardFailure=yes", "-L", sock+":127.0.0.1:9999", last)
	cmd := exec.Command(argv[0], opts...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func waitCmd(c *exec.Cmd) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- c.Wait() }()
	return ch
}

func waitForFile(path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out")
}

func openBrowser(u string) {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	default:
		name = "xdg-open"
	}
	if p, err := exec.LookPath(name); err == nil && !strings.Contains(u, " ") {
		_ = exec.Command(p, u).Start()
	}
}

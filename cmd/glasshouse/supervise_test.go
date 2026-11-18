package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/audit"
	"glasshouse/internal/backend"
	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

// fakeBackend stands in for a hypervisor: "booting" means serving an SSH
// banner on a unix socket. It lets us exercise the whole host side.
type fakeBackend struct{ proc *fakeProc }

type fakeProc struct {
	done chan struct{}
	l    net.Listener
}

func (f *fakeProc) PID() int { return os.Getpid() }
func (f *fakeProc) Wait() error {
	<-f.done
	return nil
}
func (f *fakeProc) Stop(time.Duration) error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

func (b *fakeBackend) Name() string                           { return "fake" }
func (b *fakeBackend) ImageExt() string                       { return "raw" }
func (b *fakeBackend) Preflight(*backend.Spec) error          { return nil }
func (b *fakeBackend) PrepareDisk(*backend.Spec) error        { return nil }
func (b *fakeBackend) GuestNet(backend.Spec) backend.GuestNet { return backend.GuestNet{} }
func (b *fakeBackend) OpenDisplay(backend.Spec) error         { return nil }
func (b *fakeBackend) SSHArgs(s backend.Spec, u, k string) []string {
	return []string{"ssh", u + "@fake"}
}
func (b *fakeBackend) NetdEndpoints(backend.Spec) (backend.Endpoint, backend.Endpoint) {
	return backend.Endpoint{Network: "tcp", Addr: "127.0.0.1:0"}, backend.Endpoint{Network: "tcp", Addr: "127.0.0.1:0"}
}
func (b *fakeBackend) Start(_ context.Context, s backend.Spec) (backend.Process, error) {
	l, err := net.Listen("unix", filepath.Join(s.Dir, "ssh.sock"))
	if err != nil {
		return nil, err
	}
	p := &fakeProc{done: make(chan struct{}), l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("SSH-2.0-fake\r\n"))
			c.Close()
		}
	}()
	b.proc = p
	return p, nil
}

func TestSupervisorLifecycle(t *testing.T) {
	t.Setenv("GLASSHOUSE_HOME", shortTempDir(t))
	fb := &fakeBackend{}
	testBackends["fake"] = fb
	defer delete(testBackends, "fake")

	home, _ := session.Home()
	id := "s-abc123"
	dir, err := session.Create(home, id)
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Network.Allow = []string{"good.test:443"}
	if err := writePolicySnapshot(filepath.Join(dir, "policy.json"), pol); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "disk.raw"), []byte("disk"), 0o600)
	os.WriteFile(filepath.Join(dir, "id_ed25519"), []byte("key"), 0o600)
	token := "0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := session.Save(home, session.Info{ID: id, Created: time.Now(), Backend: "fake", Arch: "amd64",
		Profile: "headless", CPUs: 2, MemoryMiB: 1024, Token: token, State: "creating", RootSSH: true}); err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() { errc <- cmdSupervise([]string{id}) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := waitReady(ctx, home, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.GatewayAddr == "" || info.ProxyAddr == "" {
		t.Fatalf("listeners not recorded: %+v", info)
	}

	// 1. CLI-side events reach the supervisor-owned audit log.
	if err := auditEvent(dir, "files.import", map[string]any{"files": 3}); err != nil {
		t.Fatal(err)
	}

	// 2. The proxy refuses an unlisted host, and a wrong token.
	connect := func(target, auth string) int {
		c, err := net.Dial("tcp", info.ProxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
		if auth != "" {
			req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("glasshouse:"+auth)) + "\r\n"
		}
		c.Write([]byte(req + "\r\n"))
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if code := connect("evil.example:443", token); code != 403 {
		t.Errorf("unlisted host: %d", code)
	}
	if code := connect("good.test:443", "wrong"); code != 407 {
		t.Errorf("wrong token: %d", code)
	}
	if code := connect("127.0.0.1:22", token); code != 403 {
		t.Errorf("ip literal / loopback: %d", code)
	}

	// 3. The gateway authenticates the placeholder key; with no real key
	//    configured it must answer 502 rather than forward anything.
	req, _ := http.NewRequest("POST", "http://"+info.GatewayAddr+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("gateway without key: %d", resp.StatusCode)
	}

	// 4. End the VM; the supervisor must clean up and finish the log.
	fb.proc.Stop(0)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("supervisor: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor did not exit")
	}
	final, _ := session.Load(home, id)
	if final.State != "stopped" || final.Token != "" {
		t.Errorf("final state %q token %q", final.State, final.Token)
	}
	for _, f := range []string{"disk.raw", "id_ed25519", "audit.sock", "ssh.sock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s should have been removed", f)
		}
	}
	n, err := audit.Verify(filepath.Join(dir, "audit.jsonl"))
	if err != nil || n < 6 {
		t.Fatalf("audit chain: n=%d err=%v", n, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	for _, want := range []string{"session.start", "session.ready", "files.import", "net.deny", "llm.error", "session.stop"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("audit log lacks %s", want)
		}
	}
	if strings.Contains(string(data), token) {
		t.Error("session token leaked into the audit log")
	}
}

func TestSupervisorRefusesCorruptAudit(t *testing.T) {
	t.Setenv("GLASSHOUSE_HOME", shortTempDir(t))
	home, _ := session.Home()
	id := "s-def456"
	dir, _ := session.Create(home, id)
	session.Save(home, session.Info{ID: id, Created: time.Now(), Backend: "fake", State: "creating"})
	os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte("{\"seq\":1,\"prev\":\"x\",\"hash\":\"y\"}\n"), 0o600)
	if err := cmdSupervise([]string{id}); err == nil {
		t.Fatal("must not start with a corrupt audit log")
	}
	in, _ := session.Load(home, id)
	if in.State != "failed" {
		t.Fatalf("state %q", in.State)
	}
}

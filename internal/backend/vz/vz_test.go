package vz

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/backend"
)

func TestBuildConfigNoNetworkDevice(t *testing.T) {
	spec := backend.Spec{Dir: "/s", CPUs: 4, MemoryMiB: 8192, Disk: "/s/d.raw", Seed: "/s/seed.iso", GUI: true,
		ProxyAddr: "/s/proxy.sock", GatewayAddr: "/s/gateway.sock"}
	c := BuildConfig(spec)
	data, _ := json.Marshal(c)
	j := string(data)
	if strings.Contains(strings.ToLower(j), "nat") || strings.Contains(strings.ToLower(j), "bridge") {
		t.Fatalf("no host network attachment may be configured: %s", j)
	}
	if len(c.Forwards) != 2 || c.Forwards[0].VsockPort != 8080 || c.Forwards[1].VsockPort != 3128 {
		t.Fatalf("forwards: %+v", c.Forwards)
	}
	spec.Offline = true
	if c := BuildConfig(spec); len(c.Forwards) != 1 {
		t.Fatalf("offline must only keep the gateway: %+v", c.Forwards)
	}
}

func TestGuestNetUsesBridges(t *testing.T) {
	g := New().GuestNet(backend.Spec{})
	if !g.VsockBridge || g.ProxyHost != "127.0.0.1" || g.GatewayURL != "http://127.0.0.1:8080" {
		t.Fatalf("%+v", g)
	}
}

func TestSSHArgsUseUnixSocket(t *testing.T) {
	a := strings.Join(New().SSHArgs(backend.Spec{Dir: "/s"}, "root", "/s/key"), " ")
	if !strings.Contains(a, "ProxyCommand=nc -U '/s/ssh.sock'") {
		t.Fatal(a)
	}
}

func TestSessionConfigNeverAttachesNetwork(t *testing.T) {
	for _, spec := range []backend.Spec{{Dir: "/s"}, {Dir: "/s", Offline: true}} {
		data, _ := json.Marshal(BuildConfig(spec))
		if strings.Contains(string(data), `"nat"`) || strings.Contains(string(data), "extraDisks") {
			t.Fatalf("allowlist/offline sessions must have no NIC and no build inputs: %s", data)
		}
	}
}

// network.mode=host: a NAT NIC, no egress proxy forward, no proxy settings in the guest; SSH and the model gateway
// still go over vsock.
func TestHostNetUsesNAT(t *testing.T) {
	spec := backend.Spec{Dir: "/s", HostNet: true, GatewayAddr: "/s/gateway.sock"}
	c := BuildConfig(spec)
	if !c.NAT {
		t.Fatal("host mode needs the NAT NIC")
	}
	if len(c.Forwards) != 1 || c.Forwards[0].VsockPort != 8080 {
		t.Fatalf("only the gateway forward may remain: %+v", c.Forwards)
	}
	g := New().GuestNet(spec)
	if g.ProxyHost != "" || !g.VsockBridge || g.GatewayURL != "http://127.0.0.1:8080" {
		t.Fatalf("%+v", g)
	}
}

// Regression: the supervisor waits for the helper in one goroutine while Stop waits in another; both must return.
func TestProcessWaitAndStopConcurrently(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	exited := make(chan struct{})
	go func() { _ = p.Wait(); close(exited) }()
	stopped := make(chan struct{})
	go func() { _ = p.Stop(time.Second); close(stopped) }()
	for _, c := range []chan struct{}{exited, stopped} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("Wait/Stop blocked after the helper exited")
		}
	}
}

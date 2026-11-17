package vz

import (
	"encoding/json"
	"strings"
	"testing"

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

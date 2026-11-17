package qemu

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/backend"
)

func spec() backend.Spec {
	return backend.Spec{
		ID: "s-abc123", Dir: "/s/dir", Arch: "amd64", CPUs: 4, MemoryMiB: 8192,
		Disk: "/s/dir/disk.qcow2", Seed: "/s/dir/seed.iso", GUI: true,
		ProxyAddr: "127.0.0.1:41000", GatewayAddr: "127.0.0.1:41001", SSHPort: 42222,
	}
}

func joined(a []string) string { return strings.Join(a, " ") }

func TestArgsSecurityFlags(t *testing.T) {
	a, err := Args(spec(), true)
	if err != nil {
		t.Fatal(err)
	}
	j := joined(a)
	for _, want := range []string{
		"-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-nodefaults", "-no-user-config",
		"restrict=on", // guest cannot reach host or outside except explicit forwards
		"hostfwd=tcp:127.0.0.1:42222-:22",
		"guestfwd=tcp:10.0.2.100:3128-tcp:127.0.0.1:41000",
		"guestfwd=tcp:10.0.2.101:8080-tcp:127.0.0.1:41001",
		"-monitor none",
		"machine q35,accel=kvm", "-cpu host",
		"virtio-gpu-pci", "-vnc unix:/s/dir/vnc.sock",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in:\n%s", want, j)
		}
	}
	for _, bad := range []string{"virgl", "venus", "gl=on", "-usb", "hostfwd=tcp::", "0.0.0.0", "-vnc :", "-vnc 127", "-virtfs", "virtiofs", "-fsdev", "-chardev", "usb-host", "vfio", "-display gtk", "-display sdl"} {
		if strings.Contains(j, bad) {
			t.Errorf("must not contain %q in:\n%s", bad, j)
		}
	}
	// the only network backend is the restricted user-mode stack
	if strings.Count(j, "-netdev") != 1 || strings.Contains(j, "tap") || strings.Contains(j, "bridge") {
		t.Errorf("unexpected network setup:\n%s", j)
	}
}

func TestArgsOfflineHasNoProxyForward(t *testing.T) {
	s := spec()
	s.Offline = true
	s.ProxyAddr = ""
	a, err := Args(s, true)
	if err != nil {
		t.Fatal(err)
	}
	j := joined(a)
	if strings.Contains(j, "3128") {
		t.Errorf("offline mode must not forward the proxy: %s", j)
	}
	if !strings.Contains(j, "guestfwd=tcp:10.0.2.101:8080") {
		t.Error("offline mode must keep the model gateway")
	}
}

func TestArgsHeadlessAndTCG(t *testing.T) {
	s := spec()
	s.GUI = false
	a, _ := Args(s, false)
	j := joined(a)
	if strings.Contains(j, "virtio-gpu") || strings.Contains(j, "-vnc") || !strings.Contains(j, "-display none") {
		t.Errorf("headless must have no display devices: %s", j)
	}
	if !strings.Contains(j, "accel=tcg") || !strings.Contains(j, "-cpu max") {
		t.Errorf("tcg flags: %s", j)
	}
}

func TestArgsArm64NeedsFirmware(t *testing.T) {
	s := spec()
	s.Arch = "arm64"
	if _, err := Args(s, true); err == nil {
		t.Fatal("arm64 without firmware must fail")
	}
	s.UEFICode, s.UEFIVars = "/fw/CODE.fd", "/s/dir/vars.fd"
	a, err := Args(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if j := joined(a); !strings.Contains(j, "machine virt,accel=kvm") || !strings.Contains(j, "if=pflash,format=raw,readonly=on,file=/fw/CODE.fd") {
		t.Errorf("arm64 args: %s", j)
	}
}

func TestArgsValidation(t *testing.T) {
	s := spec()
	s.SSHPort = 0
	if _, err := Args(s, true); err == nil {
		t.Error("missing ssh port")
	}
	s = spec()
	s.GatewayAddr = "garbage"
	if _, err := Args(s, true); err == nil {
		t.Error("bad gateway addr")
	}
	s = spec()
	s.Arch = "riscv"
	if _, err := Args(s, true); err == nil {
		t.Error("bad arch")
	}
}

func TestPreflightNoKVM(t *testing.T) {
	b := &Backend{
		LookPath:  func(n string) (string, error) { return "/usr/bin/" + n, nil },
		KVMExists: func() bool { return false },
	}
	sp := spec()
	if err := b.Preflight(&sp); err == nil || !strings.Contains(err.Error(), "kvm") {
		t.Fatalf("expected a kvm error, got %v", err)
	}
	sp.AllowTCG = true
	if err := b.Preflight(&sp); err != nil {
		t.Fatalf("tcg allowed: %v", err)
	}
}

func TestSSHArgsLoopbackOnly(t *testing.T) {
	a := New().SSHArgs(spec(), "root", "/s/dir/id_ed25519")
	j := joined(a)
	if !strings.Contains(j, "root@127.0.0.1") || !strings.Contains(j, "-p 42222") || !strings.Contains(j, "IdentitiesOnly=yes") {
		t.Fatal(j)
	}
}

func TestArgsHostNet(t *testing.T) {
	s := spec()
	s.HostNet = true
	a, err := Args(s, true)
	if err != nil {
		t.Fatal(err)
	}
	j := joined(a)
	if strings.Contains(j, "restrict=on") {
		t.Errorf("host network mode must not use restrict=on:\n%s", j)
	}
	if strings.Contains(j, "10.0.2.100") {
		t.Errorf("host network mode has no proxy forward:\n%s", j)
	}
	for _, want := range []string{"hostfwd=tcp:127.0.0.1:42222-:22", "guestfwd=tcp:10.0.2.101:8080-tcp:127.0.0.1:41001", "-sandbox on"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in:\n%s", want, j)
		}
	}
	if gn := (&Backend{}).GuestNet(s); gn.ProxyHost != "" {
		t.Errorf("host network mode must not give the guest a proxy, got %q", gn.ProxyHost)
	}
	// the default stays restricted
	d, _ := Args(spec(), true)
	if !strings.Contains(joined(d), "restrict=on") {
		t.Error("default mode must keep restrict=on")
	}
}

func TestVNCBridgeReachesTheUnixSocket(t *testing.T) {
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat not installed")
	}
	dir, err := os.MkdirTemp("", "gh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "vnc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Write([]byte("RFB 003.008\n"))
			c.Close()
		}
	}()
	port, err := New().vncBridge(sock)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 12)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "RFB 003.008\n" {
		t.Fatalf("banner through the bridge: %q %v", buf, err)
	}
	// the bridge takes one connection only: afterwards the port is closed
	time.Sleep(300 * time.Millisecond)
	if c2, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		c2.Close()
		t.Error("the bridge must close after its one connection")
	}
}

func TestVNCBridgeWithoutSocatSaysSo(t *testing.T) {
	b := New()
	b.LookPath = func(string) (string, error) { return "", errors.New("missing") }
	if _, err := b.vncBridge("/nonexistent"); err == nil || !strings.Contains(err.Error(), "socat") {
		t.Errorf("expected a socat hint, got %v", err)
	}
}

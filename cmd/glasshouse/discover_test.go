package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"glasshouse/internal/session"
)

const psSample = ` 101 /usr/bin/qemu-system-x86_64 -name glasshouse-s-66c623 -machine q35 -smp 2 -m 4096 -pidfile /srv/gh/sessions/s-66c623/qemu.pid -drive if=none,id=root,file=/srv/gh/sessions/s-66c623/disk.qcow2,format=qcow2
 102 qemu-system-x86_64 -name guest=win11,debug-threads=on -smp cpus=4,sockets=1 -m size=8G -drive file=/vm/win11.qcow2
 103 /opt/glasshouse/bin/glasshouse-vz run /Users/a/.local/share/glasshouse/sessions/s-abcdef/disk.raw --cpus 4
 104 /usr/bin/python3 -m http.server
 105 grep qemu-system
 notapid /usr/bin/qemu-system-x86_64
`

func TestParseProcesses(t *testing.T) {
	ps := parseProcesses(psSample)
	if len(ps) != 3 {
		t.Fatalf("want 3 VM processes, got %d: %+v", len(ps), ps)
	}
	a, b, c := ps[0], ps[1], ps[2]
	if a.PID != 101 || a.Name != "glasshouse-s-66c623" || a.CPUs != 2 || a.MemoryMiB != 4096 || a.ID != "s-66c623" || a.Home != "/srv/gh" {
		t.Fatalf("glasshouse qemu: %+v", a)
	}
	if b.PID != 102 || b.Name != "win11" || b.CPUs != 4 || b.MemoryMiB != 8192 || b.ID != "" {
		t.Fatalf("external qemu: %+v", b)
	}
	if c.Kind != "vz" || c.ID != "s-abcdef" || c.Home != "/Users/a/.local/share/glasshouse" {
		t.Fatalf("vz helper: %+v", c)
	}
}

func TestParseMiB(t *testing.T) {
	for in, want := range map[string]int{"2048": 2048, "2G": 2048, "512M": 512, "size=1G,slots=2": 1024, "x": 0, "": 0, "-5": 0} {
		if got := parseMiB(in); got != want {
			t.Errorf("parseMiB(%q)=%d want %d", in, got, want)
		}
	}
}

func mkSession(t *testing.T, home, id, state string) {
	t.Helper()
	if _, err := session.Create(home, id); err != nil {
		t.Fatal(err)
	}
	in := session.Info{ID: id, Created: time.Now().UTC(), Backend: "qemu", Profile: "headless", State: state, Persistent: true, CPUs: 2, MemoryMiB: 2048}
	if err := session.Save(home, in); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Dir(home, id), "disk.qcow2"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverVMs(t *testing.T) {
	cur, other := t.TempDir(), t.TempDir()
	mkSession(t, cur, "s-aaaaaa", "stopped")
	mkSession(t, other, "s-bbbbbb", "stopped")
	if err := os.MkdirAll(session.Dir(cur, "s-cccccc"), 0o700); err != nil { // no session.json: an orphan
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	oldGlobs := homeGlobs
	homeGlobs = nil
	defer func() { homeGlobs = oldGlobs }()
	psList = func() (string, error) {
		return " 7 qemu-system-x86_64 -name glasshouse-s-bbbbbb -smp 2 -m 2048 -drive file=" + session.Dir(other, "s-bbbbbb") + "/disk.qcow2\n" +
			" 8 qemu-system-x86_64 -name guest=other-vm -smp 1 -m 512\n", nil
	}
	defer func() { psList = func() (string, error) { return "", nil } }()

	homes, vms := discoverVMs(cur)
	if len(homes) != 2 {
		t.Fatalf("homes: %v", homes)
	}
	got := map[string]vmEntry{}
	for _, v := range vms {
		got[v.ID] = v
	}
	if len(vms) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(vms), vms)
	}
	if v := got["s-aaaaaa"]; v.Kind != vmSession || v.State != "stopped" || !v.Persistent || v.DiskBytes <= 0 {
		t.Fatalf("own session: %+v", v)
	}
	if v := got["s-bbbbbb"]; v.Kind != vmOther || v.PID != 7 {
		t.Fatalf("other-home session with its process: %+v", v)
	}
	if v := got["s-cccccc"]; v.Kind != vmOrphan {
		t.Fatalf("orphan: %+v", v)
	}
	if v := got["other-vm"]; v.Kind != vmExternal || v.PID != 8 || v.MemoryMiB != 512 {
		t.Fatalf("external: %+v", v)
	}
	if vms[0].State == "stopped" && vms[len(vms)-1].State != "unknown" {
		t.Fatalf("order: %+v", vms)
	}
}

func TestUIVMsEndpoint(t *testing.T) {
	psList = func() (string, error) { return "", nil }
	_, h := testUI(t, "/bin/true")
	w := do(h, "GET", "/api/vms", testAddr, nil, true, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if c := do(h, "GET", "/api/vms", testAddr, nil, false, "").Code; c != 401 {
		t.Fatalf("needs the cookie: %d", c)
	}
}

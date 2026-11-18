package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/backend"
	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

func TestCleanupKeepsDiskOfPersistentSession(t *testing.T) {
	for _, keep := range []bool{false, true} {
		dir := t.TempDir()
		files := []string{"disk.qcow2", "vars.fd", "efi-vars", "id_ed25519", "seed.iso", "qmp.sock", "audit.jsonl", "session.json"}
		for _, f := range files {
			os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
		}
		cleanup(dir, keep)
		exists := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
		for _, f := range []string{"disk.qcow2", "vars.fd", "efi-vars"} {
			if exists(f) != keep {
				t.Errorf("keep=%v: %s present=%v", keep, f, exists(f))
			}
		}
		// secrets and sockets always go; the audit log and session record always stay
		for _, f := range []string{"id_ed25519", "seed.iso", "qmp.sock"} {
			if exists(f) {
				t.Errorf("keep=%v: %s must be removed", keep, f)
			}
		}
		for _, f := range []string{"audit.jsonl", "session.json"} {
			if !exists(f) {
				t.Errorf("keep=%v: %s must stay", keep, f)
			}
		}
	}
}

func TestCheckResumable(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	spec := backend.Spec{Disk: disk}
	good := session.Info{ID: "s-abc123", Persistent: true, State: "stopped"}

	if err := checkResumable(good, spec); err == nil || !strings.Contains(err.Error(), "no disk") {
		t.Errorf("missing disk: %v", err)
	}
	os.WriteFile(disk, []byte("x"), 0o600)
	if err := checkResumable(good, spec); err != nil {
		t.Errorf("stopped kept session: %v", err)
	}
	failed := good
	failed.State = "failed"
	if err := checkResumable(failed, spec); err != nil {
		t.Errorf("failed kept session should be startable: %v", err)
	}
	for _, st := range []string{"ready", "starting"} { // dead: recorded as running but the supervisor is gone
		dead := good
		dead.State, dead.Supervisor = st, 0
		if err := checkResumable(dead, spec); err != nil {
			t.Errorf("dead session (%s) should be startable: %v", st, err)
		}
	}
	cases := map[string]func(*session.Info){
		"without --keep": func(i *session.Info) { i.Persistent = false },
		"is creating":    func(i *session.Info) { i.State = "creating" },
		"is stopping":    func(i *session.Info) { i.State = "stopping" },
		"ready and live": func(i *session.Info) { i.State, i.Supervisor = "ready", os.Getpid() },
		"already running": func(i *session.Info) {
			i.Supervisor = os.Getpid() // a live process
		},
	}
	for name, mut := range cases {
		in := good
		mut(&in)
		if err := checkResumable(in, spec); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestStartRefusesRebuiltBaseImage(t *testing.T) {
	t.Setenv("GLASSHOUSE_HOME", shortTempDir(t))
	testBackends["fake"] = &fakeBackend{}
	defer delete(testBackends, "fake")
	home, _ := session.Home()

	id := "s-abc123"
	dir, _ := session.Create(home, id)
	if err := writePolicySnapshot(filepath.Join(dir, "policy.json"), policy.Default()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "disk.raw"), []byte("disk"), 0o600)
	// the base image on disk is not the one the kept disk was created on
	base := session.ImagePath(home, "amd64", "headless", "raw")
	os.MkdirAll(filepath.Dir(base), 0o700)
	os.WriteFile(base, []byte("new image"), 0o600)
	sum := sha256.Sum256([]byte("new image"))
	os.WriteFile(base+".sha256", []byte(hex.EncodeToString(sum[:])+"  x\n"), 0o644)
	in := session.Info{ID: id, Created: time.Now(), Backend: "fake", Arch: "amd64", Profile: "headless",
		Persistent: true, State: "stopped", BaseSHA256: strings.Repeat("0", 64)}
	session.Save(home, in)

	err := cmdStart([]string{id})
	if err == nil || !strings.Contains(err.Error(), "rebuilt") {
		t.Fatalf("a rebuilt base image must stop the resume, got: %v", err)
	}
	after, _ := session.Load(home, id)
	if after.State != "stopped" || after.Boots != 0 {
		t.Errorf("session record must be untouched: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.raw")); err != nil {
		t.Error("the kept disk must not be touched")
	}
}

func TestStartNeedsExactlyOneSession(t *testing.T) {
	if err := cmdStart(nil); err == nil {
		t.Error("start without a session id must fail")
	}
}

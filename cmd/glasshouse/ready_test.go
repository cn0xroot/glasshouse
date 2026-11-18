package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/session"
)

// fakeSSH puts an `ssh` first in PATH that logs its arguments and exits with the codes in exits (one per call, the last
// one repeats). It returns the log path.
func fakeSSH(t *testing.T, exits ...string) string {
	t.Helper()
	dir := shortTempDir(t)
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\nn=$(wc -l < " + log + ")\ncase $n in\n"
	for i, c := range exits {
		script += "  " + string(rune('0'+i+1)) + ") exit " + c + " ;;\n"
	}
	script += "  *) exit " + exits[len(exits)-1] + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestGuestConfiguredScriptWaitsForWhatTheHostNeeds(t *testing.T) {
	for _, want := range []string{"cloud-init status --wait", "/workspace", "id agent", "exit 0", "exit 1"} {
		if !strings.Contains(guestConfiguredScript, want) {
			t.Errorf("script lacks %q: %s", want, guestConfiguredScript)
		}
	}
}

func TestWaitGuestConfiguredRetriesWhenSSHItselfFails(t *testing.T) {
	testBackends["fake"] = &fakeBackend{}
	defer delete(testBackends, "fake")
	// sshd is up but the key is not installed yet (ssh exits 255), twice; then the guest is done
	log := fakeSSH(t, "255", "255", "0")
	in := session.Info{ID: "s-abc123", Backend: "fake", RootSSH: true}
	if err := waitGuestConfigured(t.TempDir(), in, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	c := calls(t, log)
	if len(c) != 3 {
		t.Fatalf("want 3 ssh calls, got %d: %v", len(c), c)
	}
	if !strings.Contains(c[2], "root@fake") || !strings.Contains(c[2], "cloud-init status --wait") {
		t.Errorf("last call: %s", c[2])
	}
}

func TestWaitGuestConfiguredReturnsTheScriptsVerdictAtOnce(t *testing.T) {
	testBackends["fake"] = &fakeBackend{}
	defer delete(testBackends, "fake")
	log := fakeSSH(t, "1") // the script gave up: /workspace never appeared
	in := session.Info{ID: "s-abc123", Backend: "fake", RootSSH: true}
	err := waitGuestConfigured(t.TempDir(), in, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "first-boot setup") {
		t.Fatalf("want a setup error, got %v", err)
	}
	if n := len(calls(t, log)); n != 1 {
		t.Errorf("a script failure must not be retried, got %d calls", n)
	}
}

func TestWaitGuestConfiguredGivesUpAtTheDeadline(t *testing.T) {
	testBackends["fake"] = &fakeBackend{}
	defer delete(testBackends, "fake")
	fakeSSH(t, "255") // never authenticates
	in := session.Info{ID: "s-abc123", Backend: "fake", RootSSH: true}
	start := time.Now()
	if err := waitGuestConfigured(t.TempDir(), in, 2*time.Second); err == nil {
		t.Fatal("must fail when ssh never works")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestWaitGuestConfiguredUsesAgentWhenRootSSHIsOff(t *testing.T) {
	testBackends["fake"] = &fakeBackend{}
	defer delete(testBackends, "fake")
	log := fakeSSH(t, "0")
	in := session.Info{ID: "s-abc123", Backend: "fake", RootSSH: false}
	if err := waitGuestConfigured(t.TempDir(), in, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, log); len(c) != 1 || !strings.Contains(c[0], "agent@fake") || strings.Contains(c[0], "root@") {
		t.Errorf("ssh.root: false must use the agent user: %v", c)
	}
}

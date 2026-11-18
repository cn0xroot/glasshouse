package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"glasshouse/internal/backend"
)

func TestTunnelStderrIsAPlainFileOffMacOS(t *testing.T) {
	for _, goos := range []string{"linux", "freebsd", ""} {
		if w := tunnelStderr(goos); w != io.Writer(os.Stderr) {
			t.Errorf("%q: the tunnel's stderr must be our own stderr (a file), got %T", goos, w)
		}
	}
	if w := tunnelStderr("darwin"); w == io.Writer(os.Stderr) {
		t.Error("darwin filters the TCP_NODELAY noise, so it needs its own writer")
	}
}

func TestFilterTunnelStderrDropsOnlyTheNoise(t *testing.T) {
	in := "channel 3: open failed: connect failed: Connection refused\n" +
		"setsockopt TCP_NODELAY: Invalid argument\n" +
		"Warning: Permanently added 'x' to the list of known hosts.\n" +
		"debug: setsockopt TCP_NODELAY failed: Invalid argument\n"
	var out bytes.Buffer
	filterTunnelStderr(strings.NewReader(in), &out)
	want := "channel 3: open failed: connect failed: Connection refused\nWarning: Permanently added 'x' to the list of known hosts.\n"
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

func TestFilterTunnelStderrKeepsDrainingAfterAHugeLine(t *testing.T) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { filterTunnelStderr(r, io.Discard); close(done) }()
	go func() { w.Write([]byte(strings.Repeat("x", 2<<20) + "\nrest\n")); w.Close() }() // longer than the scan buffer
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the filter stopped reading: a writer would block forever")
	}
}

// The cleanup path of cmdMonitor: one goroutine waits for the tunnel, the cleanup kills it and receives the same
// result. With exec's copying goroutine (a non-file Stderr) two concurrent c.Wait() calls could block each other
// forever (about 1 in 60 runs); one Wait and a channel that stays readable never does.
func TestWaitCmdServesEveryReceiverWithOneWait(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := exec.Command("sleep", "30")
		pr, pw := io.Pipe()
		go io.Copy(io.Discard, pr)
		c.Stderr = pw // a writer that is not a file: exec runs a copying goroutine
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		done := waitCmd(c)
		_ = c.Process.Kill()
		first := make(chan struct{})
		go func() { <-done; close(first) }() // the select in cmdMonitor
		select {
		case <-first:
		case <-time.After(10 * time.Second):
			t.Fatal("the first receiver never got the result")
		}
		select {
		case <-done: // the deferred cleanup: must return at once
		case <-time.After(2 * time.Second):
			t.Fatalf("run %d: the second receiver blocked", i)
		}
	}
}

func TestBaseSSHOptionsNeverMultiplex(t *testing.T) {
	joined := strings.Join(backend.BaseSSHOptions("/k"), " ")
	for _, want := range []string{"ControlMaster=no", "ControlPath=none"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh options lack %q: a user's ControlMaster config could attach to another session's master (every vz session uses the host alias \"glasshouse\"): %s", want, joined)
		}
	}
}

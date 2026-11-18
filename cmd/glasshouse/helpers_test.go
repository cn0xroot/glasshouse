package main

import (
	"os"
	"testing"
)

// shortTempDir is t.TempDir under /tmp: on macOS $TMPDIR (/var/folders/...) plus the test name is too long for
// the session's unix sockets (sun_path is 104 bytes there).
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainAndReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Write("test", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	if n, err := Verify(p); err != nil || n != 3 {
		t.Fatalf("verify: %d %v", n, err)
	}
	// reopening continues the chain
	l2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	l2.Write("more", nil)
	l2.Close()
	if n, err := Verify(p); err != nil || n != 4 {
		t.Fatalf("verify after reopen: %d %v", n, err)
	}
}

func TestTamperDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(p)
	l.Write("a", map[string]any{"host": "github.com"})
	l.Write("b", map[string]any{"host": "evil.example"})
	l.Write("c", nil)
	l.Close()
	data, _ := os.ReadFile(p)

	// edit a field
	edited := strings.Replace(string(data), "evil.example", "github.com", 1)
	os.WriteFile(p, []byte(edited), 0o600)
	if _, err := Verify(p); err == nil {
		t.Fatal("edited record not detected")
	}
	// delete the middle line
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	os.WriteFile(p, []byte(lines[0]+"\n"+lines[2]+"\n"), 0o600)
	if _, err := Verify(p); err == nil {
		t.Fatal("deleted record not detected")
	}
	// a corrupt log is refused by Open (fail closed)
	if _, err := Open(p); err == nil {
		t.Fatal("Open must refuse a corrupt log")
	}
}

func TestPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(p)
	l.Close()
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

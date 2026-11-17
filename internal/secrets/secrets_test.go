package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvAndFile(t *testing.T) {
	t.Setenv("GLASSHOUSE_TEST_KEY", " sk-abc \n")
	if v, err := Resolve("env:GLASSHOUSE_TEST_KEY"); err != nil || v != "sk-abc" {
		t.Fatalf("env: %q %v", v, err)
	}
	if _, err := Resolve("env:GLASSHOUSE_NOPE"); err == nil {
		t.Fatal("unset env must fail")
	}
	d := t.TempDir()
	p := filepath.Join(d, "key")
	os.WriteFile(p, []byte("sk-file\nignored\n"), 0o600)
	if v, err := Resolve("file:" + p); err != nil || v != "sk-file" {
		t.Fatalf("file: %q %v", v, err)
	}
	os.Chmod(p, 0o644)
	if _, err := Resolve("file:" + p); err == nil {
		t.Fatal("world-readable key file must be refused")
	}
}

func TestBadRefsAndNoLeak(t *testing.T) {
	for _, r := range []string{"", "nocolon", "env:", "bogus:x"} {
		if _, err := Resolve(r); err == nil {
			t.Errorf("%q should fail", r)
		}
	}
	t.Setenv("GLASSHOUSE_TEST_KEY2", "")
	_, err := Resolve("env:GLASSHOUSE_TEST_KEY2")
	if err == nil || strings.Contains(err.Error(), "sk-") {
		t.Fatal("bad error")
	}
}

package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateLoadListRemove(t *testing.T) {
	home := t.TempDir()
	id, _ := NewID()
	if !ValidID(id) {
		t.Fatal(id)
	}
	dir, err := Create(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	if _, err := Create(home, id); err == nil {
		t.Fatal("duplicate create must fail")
	}
	in := Info{ID: id, Created: time.Now(), State: "ready", Profile: "desktop", Token: "t"}
	if err := Save(home, in); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, "session.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("session.json mode %v", st.Mode().Perm())
	}
	got, err := Load(home, id)
	if err != nil || got.Profile != "desktop" {
		t.Fatalf("%v %+v", err, got)
	}
	if l, _ := List(home); len(l) != 1 {
		t.Fatalf("list %v", l)
	}
	if err := Remove(home, id); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, id); err == nil {
		t.Fatal("removed session still loads")
	}
}

func TestInvalidIDsRejected(t *testing.T) {
	home := t.TempDir()
	for _, id := range []string{"", "../x", "s-zzzzzz", "s-1234567", "/etc", "s-12345"} {
		if ValidID(id) {
			t.Errorf("%q valid", id)
		}
		if _, err := Load(home, id); err == nil {
			t.Errorf("Load(%q) should fail", id)
		}
		if err := Remove(home, id); err == nil {
			t.Errorf("Remove(%q) should fail", id)
		}
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	if _, err := Resolve(home, ""); err == nil {
		t.Fatal("no sessions: expected error")
	}
	mk := func(id string, alive bool) {
		Create(home, id)
		in := Info{ID: id, Created: time.Now(), State: "ready"}
		if alive {
			in.Supervisor = os.Getpid()
		}
		Save(home, in)
	}
	mk("s-aaaaaa", false)
	if _, err := Resolve(home, ""); err == nil {
		t.Fatal("dead session must not resolve")
	}
	mk("s-bbbbbb", true)
	if in, err := Resolve(home, ""); err != nil || in.ID != "s-bbbbbb" {
		t.Fatalf("%v %v", in.ID, err)
	}
	mk("s-cccccc", true)
	if _, err := Resolve(home, ""); err == nil {
		t.Fatal("ambiguous must error")
	}
	if in, err := Resolve(home, "s-aaaaaa"); err != nil || in.ID != "s-aaaaaa" {
		t.Fatal("explicit id")
	}
}

func TestTokenShape(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if len(a) != 48 || a == b {
		t.Fatalf("%q %q", a, b)
	}
}

func TestVerifyImage(t *testing.T) {
	d := t.TempDir()
	img := filepath.Join(d, "base.qcow2")
	os.WriteFile(img, []byte("hello"), 0o600)
	if err := VerifyImage(img); err == nil {
		t.Fatal("missing sidecar must fail")
	}
	// sha256("hello")
	os.WriteFile(img+".sha256", []byte("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824  base.qcow2\n"), 0o600)
	if err := VerifyImage(img); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(img, []byte("tampered"), 0o600)
	if err := VerifyImage(img); err == nil {
		t.Fatal("tampered image must fail")
	}
}

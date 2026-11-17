package fsbridge

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDenied(t *testing.T) {
	pats := []string{".env*", "*.pem", "id_*", ".git/config", "secrets/*", "node_modules"}
	yes := []string{".env", "app/.env.local", "a/b/x.pem", "id_rsa", ".git/config", "sub/.git/config",
		"secrets/a", "secrets/deep/a", "node_modules/x/y.js"}
	no := []string{"main.go", "docs/environment.md", "pem.txt", ".git/HEAD", "my_secrets/a", "src/id.go"}
	for _, p := range yes {
		if !Denied(p, pats) {
			t.Errorf("%s should be denied", p)
		}
	}
	for _, p := range no {
		if Denied(p, pats) {
			t.Errorf("%s should be allowed", p)
		}
	}
}

func TestPackSkipsSecretsAndLinks(t *testing.T) {
	src := t.TempDir()
	write(t, src, "main.go", "package main", 0o644)
	write(t, src, "sub/ok.txt", "ok", 0o755)
	write(t, src, ".env", "TOKEN=1", 0o600)
	write(t, src, "keys/server.pem", "KEY", 0o600)
	os.Symlink("sub/ok.txt", filepath.Join(src, "link-in"))
	os.Symlink("/etc/passwd", filepath.Join(src, "link-abs"))
	os.Symlink("../outside", filepath.Join(src, "link-up"))

	var buf bytes.Buffer
	rep, err := Pack(src, &buf, Options{DenyPatterns: []string{".env*", "*.pem"}})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names[strings.TrimSuffix(h.Name, "/")] = true
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s: ownership not zeroed", h.Name)
		}
	}
	for _, want := range []string{"main.go", "sub", "sub/ok.txt", "link-in", "keys"} {
		if !names[want] {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	for _, bad := range []string{".env", "keys/server.pem", "link-abs", "link-up"} {
		if names[bad] {
			t.Errorf("%s must not be packed", bad)
		}
	}
	if rep.Files != 2 || len(rep.Skipped) < 4 {
		t.Errorf("report %+v", rep)
	}
}

func TestPackLimits(t *testing.T) {
	src := t.TempDir()
	write(t, src, "big", strings.Repeat("x", 1000), 0o644)
	if _, err := Pack(src, &bytes.Buffer{}, Options{MaxBytes: 500}); err == nil {
		t.Fatal("size limit not enforced")
	}
	write(t, src, "b", "1", 0o644)
	if _, err := Pack(src, &bytes.Buffer{}, Options{MaxFiles: 1}); err == nil {
		t.Fatal("file limit not enforced")
	}
}

type ent struct {
	name, link, body string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, ents ...ent) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Linkname: e.link, Mode: e.mode, ModTime: time.Now()}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return &b
}

func TestExtractRejectsEscapes(t *testing.T) {
	for name, e := range map[string]ent{
		"dotdot":    {name: "../evil"},
		"nested":    {name: "a/../../evil"},
		"abs":       {name: "/etc/cron.d/x"},
		"backslash": {name: "..\\evil"},
	} {
		dst := t.TempDir()
		if _, err := Extract(mkTar(t, e), dst, Options{}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExtractSymlinkTricks(t *testing.T) {
	dst := t.TempDir()
	outside := t.TempDir()
	// a symlink pointing outside is skipped, so the follow-up write cannot go through it
	rep, err := Extract(mkTar(t,
		ent{name: "evil", typ: tar.TypeSymlink, link: outside},
		ent{name: "evil/pwned", body: "x"},
	), dst, Options{})
	if err == nil {
		// writing "evil/pwned" must fail because "evil" does not exist as a dir or is not plain dir
		if _, serr := os.Stat(filepath.Join(outside, "pwned")); serr == nil {
			t.Fatal("wrote through a symlink!")
		}
	}
	if _, serr := os.Stat(filepath.Join(outside, "pwned")); serr == nil {
		t.Fatal("wrote through a symlink!")
	}
	_ = rep

	// an inside symlink followed by a write through it is refused
	dst2 := t.TempDir()
	os.MkdirAll(filepath.Join(dst2, "real"), 0o755)
	_, err = Extract(mkTar(t,
		ent{name: "real/", typ: tar.TypeDir},
		ent{name: "ln", typ: tar.TypeSymlink, link: "real"},
		ent{name: "ln/file", body: "x"},
	), dst2, Options{})
	if err == nil {
		t.Fatal("write through an in-tree symlink must be refused")
	}

	// absolute and escaping links are dropped
	dst3 := t.TempDir()
	rep3, err := Extract(mkTar(t,
		ent{name: "abs", typ: tar.TypeSymlink, link: "/etc/passwd"},
		ent{name: "up", typ: tar.TypeSymlink, link: "../../x"},
	), dst3, Options{})
	if err != nil || len(rep3.Skipped) != 2 {
		t.Fatalf("%v %+v", err, rep3)
	}
	if _, err := os.Lstat(filepath.Join(dst3, "abs")); err == nil {
		t.Fatal("abs link created")
	}
}

func TestExtractSpecialAndModes(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(mkTar(t,
		ent{name: "dev", typ: tar.TypeChar},
		ent{name: "hl", typ: tar.TypeLink, link: "x"},
		ent{name: "fifo", typ: tar.TypeFifo},
		ent{name: "suid", body: "x", mode: 0o4755},
		ent{name: "ro", body: "x", mode: 0o000},
	), dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 3 {
		t.Fatalf("skipped %v", rep.Skipped)
	}
	st, _ := os.Stat(filepath.Join(dst, "suid"))
	if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Mode().Perm() != 0o755 {
		t.Fatalf("suid mode %v", st.Mode())
	}
	st, _ = os.Stat(filepath.Join(dst, "ro"))
	if st.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("owner must be able to read/write: %v", st.Mode())
	}
}

func TestExtractLimitsAndDeny(t *testing.T) {
	dst := t.TempDir()
	if _, err := Extract(mkTar(t, ent{name: "a", body: strings.Repeat("x", 100)}), dst, Options{MaxBytes: 50}); err == nil {
		t.Fatal("size limit")
	}
	dst = t.TempDir()
	rep, err := Extract(mkTar(t,
		ent{name: ".git/config", body: "[remote]"},
		ent{name: "src/main.go", body: "ok"},
		ent{name: "src/.env", body: "S=1"},
	), dst, Options{DenyPatterns: []string{".git/config", ".env*"}})
	if err != nil || rep.Files != 1 || len(rep.Skipped) != 2 {
		t.Fatalf("%v %+v", err, rep)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "config")); err == nil {
		t.Fatal("denied file extracted")
	}
}

func TestRoundTripDiffApply(t *testing.T) {
	proj := t.TempDir()
	write(t, proj, "keep.txt", "same", 0o644)
	write(t, proj, "edit.txt", "old", 0o644)
	write(t, proj, "gone.txt", "bye", 0o644)
	write(t, proj, ".env", "SECRET", 0o600)

	// "guest" tree: edit one, add one (executable), drop one
	guest := t.TempDir()
	write(t, guest, "keep.txt", "same", 0o644)
	write(t, guest, "edit.txt", "new", 0o644)
	write(t, guest, "new/run.sh", "#!/bin/sh", 0o755)
	write(t, guest, ".env", "GUEST-CHANGED", 0o600) // must be ignored by deny patterns

	var buf bytes.Buffer
	deny := []string{".env*"}
	if _, err := Pack(guest, &buf, Options{DenyPatterns: deny}); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := Extract(&buf, staging, Options{DenyPatterns: deny}); err != nil {
		t.Fatal(err)
	}
	changes, err := Diff(proj, staging, deny, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range changes {
		got[c.Path] = c
	}
	if got["edit.txt"].Kind != Modified || got["new/run.sh"].Kind != Added || !got["new/run.sh"].ExecBit ||
		got["gone.txt"].Kind != Deleted || len(got) != 3 {
		t.Fatalf("changes: %+v", changes)
	}
	if err := Apply(staging, proj, changes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(proj, "edit.txt"))
	if string(b) != "new" {
		t.Fatal("edit not applied")
	}
	if _, err := os.Stat(filepath.Join(proj, "gone.txt")); err == nil {
		t.Fatal("delete not applied")
	}
	if b, _ := os.ReadFile(filepath.Join(proj, ".env")); string(b) != "SECRET" {
		t.Fatal(".env must be untouched")
	}
}

func TestApplyRefusesSymlinkedParent(t *testing.T) {
	proj := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(proj, "linked"))
	staging := t.TempDir()
	write(t, staging, "linked/x.txt", "pwn", 0o644)
	err := Apply(staging, proj, []Change{{Path: "linked/x.txt", Kind: Added}})
	if err == nil {
		t.Fatal("Apply followed a symlinked parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("wrote outside the project")
	}
}

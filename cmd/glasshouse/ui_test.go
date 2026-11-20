package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAddr = "127.0.0.1:4321"

func testUI(t *testing.T, self string) (*uiServer, http.Handler) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GLASSHOUSE_HOME", home)
	s := newUIServer(home, self, "tok123", testAddr)
	return s, s.handler()
}

func do(h http.Handler, method, path, host string, hdr map[string]string, cookie bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if cookie {
		r.AddCookie(&http.Cookie{Name: uiCookie, Value: "tok123"})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUIGuard(t *testing.T) {
	_, h := testUI(t, "/bin/true")
	if c := do(h, "GET", "/api/sessions", testAddr, nil, false, "").Code; c != 401 {
		t.Fatalf("no cookie: %d", c)
	}
	if c := do(h, "GET", "/", testAddr, nil, true, "").Code; c != 200 {
		t.Fatalf("page: %d", c)
	}
	if c := do(h, "GET", "/api/sessions", "evil.example:4321", nil, true, "").Code; c != 403 {
		t.Fatalf("bad host (DNS rebinding): %d", c)
	}
	if c := do(h, "GET", "/api/sessions", "localhost:4321", nil, true, "").Code; c != 200 {
		t.Fatalf("localhost host: %d", c)
	}
	w := do(h, "GET", "/?t=tok123", testAddr, nil, false, "")
	if w.Code != 302 || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(w.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("token exchange: %d %v", w.Code, w.Header())
	}
	if c := do(h, "GET", "/?t=wrong", testAddr, nil, false, "").Code; c != 401 {
		t.Fatalf("wrong token: %d", c)
	}
	for name, hdr := range map[string]map[string]string{
		"no header":  nil,
		"bad origin": {"X-Glasshouse-UI": "1", "Origin": "http://evil.example"},
	} {
		if c := do(h, "POST", "/api/run", testAddr, hdr, true, `{"mode":"x"}`).Code; c != 403 {
			t.Fatalf("POST %s: %d", name, c)
		}
	}
	ok := map[string]string{"X-Glasshouse-UI": "1", "Origin": "http://" + testAddr}
	if c := do(h, "POST", "/api/run", testAddr, ok, true, `{"mode":"x"}`).Code; c != 400 {
		t.Fatalf("valid POST reaches the handler: %d", c)
	}
}

func TestBuildRunArgs(t *testing.T) {
	dir := t.TempDir()
	got, err := buildRunArgs(runReq{Mode: "shell", Project: dir, Offline: true, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--shell", "--offline", "--keep", dir, "--", "true"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	if got, _ = buildRunArgs(runReq{Mode: "gui"}); strings.Join(got, "|") != "run|--gui" {
		t.Fatalf("gui: %v", got)
	}
	for name, req := range map[string]runReq{
		"mode":        {Mode: "root"},
		"flag-like":   {Mode: "shell", Project: "--policy=/etc/passwd"},
		"missing dir": {Mode: "shell", Project: filepath.Join(dir, "nope")},
	} {
		if _, err := buildRunArgs(req); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestUISessionIDValidation(t *testing.T) {
	_, h := testUI(t, "/bin/true")
	ok := map[string]string{"X-Glasshouse-UI": "1"}
	for _, id := range []string{"..", "s-12345", "s-ZZZZZZ", "s-123456;id", "x-123456"} {
		if c := do(h, "POST", "/api/session/"+id+"/stop", testAddr, ok, true, "").Code; c != 400 && c != 404 && c != 307 { // 307: the mux cleans ".." out of the path and redirects, nothing runs
			t.Fatalf("%q accepted: %d", id, c)
		}
	}
	if c := do(h, "POST", "/api/session/s-123456/format", testAddr, ok, true, "").Code; c != 404 {
		t.Fatalf("unknown action: %d", c)
	}
}

func TestUIJobRunsOwnBinaryOnly(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-glasshouse")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"args: $*\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, h := testUI(t, fake)
	ok := map[string]string{"X-Glasshouse-UI": "1"}
	w := do(h, "POST", "/api/session/s-0a1b2c/stop", testAddr, ok, true, "")
	if w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	var r struct{ Job string }
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	var job uiJobView
	for i := 0; i < 100; i++ {
		jw := do(h, "GET", "/api/jobs/"+r.Job, testAddr, nil, true, "")
		_ = json.Unmarshal(jw.Body.Bytes(), &job)
		if job.Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !job.Done || !job.OK || strings.TrimSpace(job.Output) != "args: stop s-0a1b2c" {
		t.Fatalf("job: %+v", job)
	}
}

func TestUISessionsEmpty(t *testing.T) {
	_, h := testUI(t, "/bin/true")
	w := do(h, "GET", "/api/sessions", testAddr, nil, true, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
}

func TestLimitedBufKeepsTail(t *testing.T) {
	var b limitedBuf
	b.Write([]byte(strings.Repeat("a", 70<<10)))
	b.Write([]byte("END"))
	if len(b.b) != 64<<10 || !strings.HasSuffix(string(b.b), "END") {
		t.Fatalf("len %d", len(b.b))
	}
}

// The settings dialog offers the CC-Monitor palettes and fonts; a rename in one place must not silently drop one.
func TestUIPageHasThemesFontsAndLanguages(t *testing.T) {
	page := string(uiPage)
	for _, th := range []string{"brand", "light", "dark", "dracula", "nord", "midnight", "ocean", "forest", "sunset", "rose"} {
		if !strings.Contains(page, `:root[data-theme="`+th+`"]`) {
			t.Errorf("theme %q has no palette", th)
		}
		if !strings.Contains(page, "th_"+th+":") {
			t.Errorf("theme %q has no label", th)
		}
	}
	for _, f := range []string{"system", "mono", "serif", "rounded", "kaiti", "heiti", "songti"} {
		if !strings.Contains(page, f+":'") || !strings.Contains(page, "f_"+f+":") {
			t.Errorf("font %q is incomplete", f)
		}
	}
	for _, k := range []string{`value="auto"`, `value="zh"`, `value="en"`} {
		if !strings.Contains(page, k) {
			t.Errorf("language option %s missing", k)
		}
	}
}

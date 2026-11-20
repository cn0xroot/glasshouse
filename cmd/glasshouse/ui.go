package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"glasshouse/internal/session"
)

//go:embed ui/index.html
var uiPage []byte

// `glasshouse ui` is a local control panel in the browser. It only ever listens on 127.0.0.1, needs a random token,
// checks the Host and Origin headers (DNS rebinding, cross-site requests) and never runs anything but glasshouse's own
// subcommands with validated arguments, so the page cannot be used to run arbitrary commands on the host.

var uiSessionID = regexp.MustCompile(`^s-[0-9a-f]{6}$`)

const uiCookie = "gh_ui"

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	port := fs.Int("port", 0, "TCP port on 127.0.0.1 (default: a free port)")
	noOpen := fs.Bool("no-open", false, "do not open the browser, only print the URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	srv := newUIServer(home, self, hex.EncodeToString(tok), ln.Addr().String())
	url := fmt.Sprintf("http://%s/?t=%s", ln.Addr(), srv.token)
	fmt.Println("Glasshouse control panel:", url)
	fmt.Println("listening on 127.0.0.1 only; press Ctrl+C to stop (sessions keep running)")
	if !*noOpen {
		if err := openURL(url); err != nil {
			fmt.Fprintln(os.Stderr, "note: could not open a browser:", err)
		}
	}
	hs := &http.Server{Handler: srv.handler(), ReadHeaderTimeout: 10 * time.Second}
	return hs.Serve(ln)
}

func openURL(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return exec.Command(path, u).Start()
}

type uiServer struct {
	home, self, token, addr string

	mu   sync.Mutex
	jobs map[string]*uiJob
	seq  int
}

func newUIServer(home, self, token, addr string) *uiServer {
	return &uiServer{home: home, self: self, token: token, addr: addr, jobs: map[string]*uiJob{}}
}

// uiJob is a background glasshouse command; uiJobView is what the page sees.
type uiJob struct {
	ID      string
	Title   string
	Started time.Time
	Done    bool
	OK      bool
	mu      sync.Mutex
	buf     limitedBuf
}

type uiJobView struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Started time.Time `json:"started"`
	Done    bool      `json:"done"`
	OK      bool      `json:"ok"`
	Output  string    `json:"output"`
}

// limitedBuf keeps the last 64 KiB of a command's output.
type limitedBuf struct{ b []byte }

func (l *limitedBuf) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > 64<<10 {
		l.b = l.b[len(l.b)-64<<10:]
	}
	return len(p), nil
}

type jobWriter struct{ j *uiJob }

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.mu.Lock()
	defer w.j.mu.Unlock()
	return w.j.buf.Write(p)
}

func (s *uiServer) startJob(title string, timeout time.Duration, args ...string) *uiJob {
	s.mu.Lock()
	s.seq++
	j := &uiJob{ID: fmt.Sprintf("j%d", s.seq), Title: title, Started: time.Now()}
	s.jobs[j.ID] = j
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.self, args...)
		cmd.Stdin = nil
		cmd.Stdout, cmd.Stderr = jobWriter{j}, jobWriter{j}
		err := cmd.Run()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.Done, j.OK = true, err == nil
		if err != nil {
			fmt.Fprintf(&j.buf, "\n%v\n", err)
		}
	}()
	return j
}

func (j *uiJob) snapshot() uiJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return uiJobView{ID: j.ID, Title: j.Title, Started: j.Started, Done: j.Done, OK: j.OK, Output: string(j.buf.b)}
}

// capture runs a short glasshouse subcommand and returns its combined output.
func (s *uiServer) capture(timeout time.Duration, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.self, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (s *uiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /api/sessions", s.apiSessions)
	mux.HandleFunc("POST /api/run", s.apiRun)
	mux.HandleFunc("POST /api/session/{id}/{action}", s.apiAction)
	mux.HandleFunc("GET /api/session/{id}/logs", s.apiLogs)
	mux.HandleFunc("GET /api/session/{id}/pull", s.apiPullPreview)
	mux.HandleFunc("POST /api/session/{id}/pull", s.apiPullApply)
	mux.HandleFunc("GET /api/vms", s.apiVMs)
	mux.HandleFunc("GET /api/doctor", s.apiDoctor)
	mux.HandleFunc("GET /api/jobs", s.apiJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.apiJob)
	return s.guard(mux)
}

// guard enforces the Host check, the token cookie and, for writes, a custom header that a cross-site page cannot send.
func (s *uiServer) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.addr)
	okHost := map[string]bool{s.addr: true, "localhost:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !okHost[r.Host] {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
		if r.Method == http.MethodGet && r.URL.Path == "/" && s.tokenEq(r.URL.Query().Get("t")) {
			http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		c, err := r.Cookie(uiCookie)
		if err != nil || !s.tokenEq(c.Value) {
			http.Error(w, "open the URL printed by `glasshouse ui` (it carries the access token)", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			if r.Header.Get("X-Glasshouse-UI") != "1" {
				http.Error(w, "missing header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "bad origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *uiServer) tokenEq(v string) bool {
	return subtle.ConstantTimeCompare([]byte(v), []byte(s.token)) == 1
}

func (s *uiServer) servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiPage)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type uiSession struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Backend    string `json:"backend"`
	Profile    string `json:"profile"`
	Age        string `json:"age"`
	Egress     string `json:"egress"`
	LLM        string `json:"llm"`
	Project    string `json:"project"`
	Persistent bool   `json:"persistent"`
	GUI        bool   `json:"gui"`
	CPUs       int    `json:"cpus"`
	MemoryMiB  int    `json:"memory_mib"`
	HostNet    bool   `json:"host_net"`
	Offline    bool   `json:"offline"`
	Error      string `json:"error,omitempty"`
}

func (s *uiServer) apiSessions(w http.ResponseWriter, r *http.Request) {
	all, err := session.List(s.home)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Created.After(all[j].Created) })
	out := make([]uiSession, 0, len(all))
	for _, in := range all {
		state := in.State
		if (state == "ready" || state == "starting") && !in.Alive() {
			state = "dead"
		}
		eg, llm := "-", "-"
		if st, err := session.LoadStats(s.home, in.ID); err == nil {
			eg, llm = humanBytes(st.Egress), fmt.Sprint(st.LLMRequests)
		}
		out = append(out, uiSession{ID: in.ID, State: state, Backend: in.Backend, Profile: in.Profile, Age: humanAge(in.Created),
			Egress: eg, LLM: llm, Project: in.Project, Persistent: in.Persistent, GUI: in.GUI, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB,
			HostNet: in.HostNet, Offline: in.Offline, Error: in.Error})
	}
	writeJSON(w, 200, out)
}

type runReq struct {
	Mode    string `json:"mode"` // shell | gui
	Project string `json:"project"`
	Offline bool   `json:"offline"`
	Keep    bool   `json:"keep"`
}

func (s *uiServer) apiRun(w http.ResponseWriter, r *http.Request) {
	var req runReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	args, err := buildRunArgs(req)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	title := "run --" + req.Mode
	if req.Project != "" {
		title += " " + req.Project
	}
	j := s.startJob(title, 15*time.Minute, args...)
	writeJSON(w, 202, map[string]string{"job": j.ID})
}

// buildRunArgs turns the form into glasshouse run arguments. Shell mode ends with `-- true`: without a command it would
// wait for an interactive terminal, and the session keeps running after the command anyway.
func buildRunArgs(req runReq) ([]string, error) {
	var args []string
	switch req.Mode {
	case "shell":
		args = []string{"run", "--shell"}
	case "gui":
		args = []string{"run", "--gui"}
	default:
		return nil, errors.New("mode must be shell or gui")
	}
	if req.Offline {
		args = append(args, "--offline")
	}
	if req.Keep {
		args = append(args, "--keep")
	}
	if p := strings.TrimSpace(req.Project); p != "" {
		if strings.HasPrefix(p, "-") {
			return nil, errors.New("project must be a directory path")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", p)
		}
		args = append(args, abs)
	}
	if req.Mode == "shell" {
		args = append(args, "--", "true")
	}
	return args, nil
}

func (s *uiServer) apiAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if !uiSessionID.MatchString(id) {
		jsonErr(w, 400, "bad session id")
		return
	}
	switch action {
	case "stop":
		j := s.startJob("stop "+id, 3*time.Minute, "stop", id)
		writeJSON(w, 202, map[string]string{"job": j.ID})
	case "rm":
		j := s.startJob("rm "+id, time.Minute, "rm", id)
		writeJSON(w, 202, map[string]string{"job": j.ID})
	case "start":
		j := s.startJob("start "+id, 15*time.Minute, "start", id, "--", "true")
		writeJSON(w, 202, map[string]string{"job": j.ID})
	case "gui":
		j := s.startJob("gui "+id, time.Minute, "gui", id)
		writeJSON(w, 202, map[string]string{"job": j.ID})
	case "terminal":
		if err := openTerminal(s.self, id); err != nil {
			jsonErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "terminal opened"})
	default:
		jsonErr(w, 404, "unknown action")
	}
}

// openTerminal opens a terminal window running `glasshouse ssh <id>`.
func openTerminal(self, id string) error {
	cmdline := shellQuote(self) + " ssh " + id
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`tell application "Terminal" to do script %q`, cmdline)
		script += `
tell application "Terminal" to activate`
		return exec.Command("osascript", "-e", script).Start()
	}
	type term struct {
		name string
		args []string
	}
	for _, t := range []term{
		{"x-terminal-emulator", []string{"-e", "sh", "-c", cmdline}},
		{"gnome-terminal", []string{"--", "sh", "-c", cmdline}},
		{"konsole", []string{"-e", "sh", "-c", cmdline}},
		{"xfce4-terminal", []string{"-x", "sh", "-c", cmdline}},
		{"xterm", []string{"-e", "sh", "-c", cmdline}},
	} {
		if p, err := exec.LookPath(t.name); err == nil {
			return exec.Command(p, t.args...).Start()
		}
	}
	return errors.New("no terminal emulator found; run in a terminal: glasshouse ssh " + id)
}

func (s *uiServer) apiLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uiSessionID.MatchString(id) {
		jsonErr(w, 400, "bad session id")
		return
	}
	args := []string{"logs", id}
	switch r.URL.Query().Get("kind") {
	case "verify":
		args = []string{"logs", "--verify", id}
	case "console":
		args = []string{"logs", "--console", id}
	}
	out, err := s.capture(30*time.Second, "", args...)
	writeJSON(w, 200, map[string]any{"output": out, "ok": err == nil})
}

// apiPullPreview shows what a pull would change: it answers "n" to the confirmation.
func (s *uiServer) apiPullPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uiSessionID.MatchString(id) {
		jsonErr(w, 400, "bad session id")
		return
	}
	out, err := s.capture(2*time.Minute, "n\n", "pull", id)
	writeJSON(w, 200, map[string]any{"output": out, "ok": err == nil})
}

func (s *uiServer) apiPullApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uiSessionID.MatchString(id) {
		jsonErr(w, 400, "bad session id")
		return
	}
	out, err := s.capture(5*time.Minute, "y\n", "pull", id)
	writeJSON(w, 200, map[string]any{"output": out, "ok": err == nil})
}

func (s *uiServer) apiDoctor(w http.ResponseWriter, r *http.Request) {
	out, err := s.capture(time.Minute, "", "doctor")
	writeJSON(w, 200, map[string]any{"output": out, "ok": err == nil})
}

func (s *uiServer) apiJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := make([]uiJobView, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j.snapshot())
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, k int) bool { return list[i].Started.After(list[k].Started) })
	if len(list) > 20 {
		list = list[:20]
	}
	writeJSON(w, 200, list)
}

func (s *uiServer) apiJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		jsonErr(w, 404, "no such job")
		return
	}
	writeJSON(w, 200, j.snapshot())
}

// apiVMs lists the virtual machines found on this machine (see discoverVMs).
func (s *uiServer) apiVMs(w http.ResponseWriter, r *http.Request) {
	homes, vms := discoverVMs(s.home)
	if vms == nil {
		vms = []vmEntry{}
	}
	writeJSON(w, 200, map[string]any{"home": s.home, "homes": homes, "vms": vms})
}

// Package session owns the on-disk state of sandbox sessions. Everything lives
// under one private directory per session that the guest can never reach.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Info struct {
	ID          string    `json:"id"`
	Created     time.Time `json:"created"`
	Backend     string    `json:"backend"`
	Arch        string    `json:"arch"`
	Profile     string    `json:"profile"`
	Project     string    `json:"project,omitempty"`
	Persistent  bool      `json:"persistent"`
	GUI         bool      `json:"gui"`
	Offline     bool      `json:"offline"`
	HostNet     bool      `json:"host_net,omitempty"`
	CCMonitor   bool      `json:"cc_monitor"`
	CPUs        int       `json:"cpus"`
	MemoryMiB   int       `json:"memory_mib"`
	DiskBytes   int64     `json:"disk_bytes"`
	Token       string    `json:"token"` // proxy password / guest placeholder API key
	State       string    `json:"state"` // creating | starting | ready | stopping | stopped | failed
	Error       string    `json:"error,omitempty"`
	Supervisor  int       `json:"supervisor,omitempty"`
	SSHPort     int       `json:"ssh_port,omitempty"`
	ProxyAddr   string    `json:"proxy_addr,omitempty"`
	GatewayAddr string    `json:"gateway_addr,omitempty"`
	RootSSH     bool      `json:"root_ssh"`
	AllowTCG    bool      `json:"allow_tcg,omitempty"`
	// Boots counts resumes of a kept session (glasshouse start); BaseSHA256 pins the base image the kept disk is layered on.
	Boots      int    `json:"boots,omitempty"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
}

type Stats struct {
	Egress      int64     `json:"egress"`
	Ingress     int64     `json:"ingress"`
	LLMRequests int64     `json:"llm_requests"`
	Updated     time.Time `json:"updated"`
}

var idRe = regexp.MustCompile(`^s-[0-9a-f]{6}$`)

func ValidID(id string) bool { return idRe.MatchString(id) }

// Home is $GLASSHOUSE_HOME or ~/.local/share/glasshouse.
func Home() (string, error) {
	if h := os.Getenv("GLASSHOUSE_HOME"); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".local", "share", "glasshouse"), nil
}

func SessionsDir(home string) string { return filepath.Join(home, "sessions") }
func Dir(home, id string) string     { return filepath.Join(home, "sessions", id) }

func NewID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "s-" + hex.EncodeToString(b), nil
}

func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil // 48 hex chars
}

// Create makes the private session directory (0700).
func Create(home, id string) (string, error) {
	if !ValidID(id) {
		return "", errors.New("invalid session id")
	}
	if err := os.MkdirAll(SessionsDir(home), 0o700); err != nil {
		return "", err
	}
	dir := Dir(home, id)
	if err := CheckSocketDir(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// longestSocket is the longest unix socket name created inside a session directory.
const longestSocket = "gateway.sock"

// SocketPathMax is the usable length of a unix socket path: sizeof(sun_path) minus the terminating NUL.
// macOS (104) is shorter than Linux (108), so a GLASSHOUSE_HOME that works on Linux can fail on a Mac.
func SocketPathMax() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// CheckSocketDir refuses a session directory whose sockets would not fit in sun_path, with an error that
// says how to fix it, instead of the bare "bind: invalid argument" the kernel would give later.
func CheckSocketDir(dir string) error {
	p := filepath.Join(dir, longestSocket)
	if len(p) > SocketPathMax() {
		return fmt.Errorf("session directory path is too long for unix sockets on %s (%d > %d bytes: %s); set GLASSHOUSE_HOME to a shorter directory",
			runtime.GOOS, len(p), SocketPathMax(), p)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Save(home string, in Info) error {
	return writeJSON(filepath.Join(Dir(home, in.ID), "session.json"), in)
}

func Load(home, id string) (Info, error) {
	if !ValidID(id) {
		return Info{}, fmt.Errorf("invalid session id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(Dir(home, id), "session.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, fmt.Errorf("no such session %s", id)
		}
		return Info{}, err
	}
	var in Info
	return in, json.Unmarshal(b, &in)
}

func SaveStats(home, id string, s Stats) error {
	return writeJSON(filepath.Join(Dir(home, id), "stats.json"), s)
}

func LoadStats(home, id string) (Stats, error) {
	var s Stats
	b, err := os.ReadFile(filepath.Join(Dir(home, id), "stats.json"))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func List(home string) ([]Info, error) {
	ents, err := os.ReadDir(SessionsDir(home))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		if in, err := Load(home, e.Name()); err == nil {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Alive reports whether the supervisor process still exists.
func (in Info) Alive() bool {
	if in.Supervisor <= 0 {
		return false
	}
	return syscall.Kill(in.Supervisor, 0) == nil
}

// Resolve picks the session to act on: the given id, or the only live one.
func Resolve(home, id string) (Info, error) {
	if id != "" {
		return Load(home, id)
	}
	all, err := List(home)
	if err != nil {
		return Info{}, err
	}
	var live []Info
	for _, s := range all {
		if s.Alive() && (s.State == "ready" || s.State == "starting") {
			live = append(live, s)
		}
	}
	switch len(live) {
	case 0:
		return Info{}, errors.New("no running session; start one with `glasshouse run`")
	case 1:
		return live[0], nil
	}
	ids := make([]string, len(live))
	for i, s := range live {
		ids[i] = s.ID
	}
	return Info{}, fmt.Errorf("several sessions are running (%s); name one", strings.Join(ids, ", "))
}

func Remove(home, id string) error {
	if !ValidID(id) {
		return errors.New("invalid session id")
	}
	return os.RemoveAll(Dir(home, id))
}

// GenKey creates the session's ephemeral ed25519 key with ssh-keygen and
// returns the public key line.
func GenKey(dir, id string) (pub string, err error) {
	priv := filepath.Join(dir, "id_ed25519")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "glasshouse-"+id, "-f", priv).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(priv + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func ImagePath(home, arch, profile, ext string) string {
	return filepath.Join(home, "images", fmt.Sprintf("ubuntu-26.04-%s-%s.%s", arch, profile, ext))
}

// VerifyImage checks the template against its <image>.sha256 sidecar.
func VerifyImage(path string) error {
	side, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return fmt.Errorf("missing checksum file %s.sha256 (images must be built with %s)", filepath.Base(path), buildScript())
	}
	fields := strings.Fields(string(side))
	if len(fields) == 0 {
		return errors.New("empty checksum file")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("base image not found: %s (build it with %s)", path, buildScript())
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, fields[0]) {
		return fmt.Errorf("base image checksum mismatch: the template was modified")
	}
	return nil
}

// buildScript names the image build script for this host.
func buildScript() string {
	if runtime.GOOS == "darwin" {
		return "image/build-mac.sh"
	}
	return "image/build.sh"
}

// ImageSHA returns the checksum recorded in the image's .sha256 sidecar. Call it after VerifyImage, which proves the
// image matches that checksum.
func ImageSHA(path string) (string, error) {
	side, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(side))
	if len(f) == 0 {
		return "", errors.New("empty checksum file")
	}
	return strings.ToLower(f[0]), nil
}

// Package policy loads, validates and tightens Glasshouse policy files.
// The user policy sets the ceiling; a project file (.glasshouse.yaml) may only
// make things stricter, never looser. Unknown keys are errors.
package policy

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"glasshouse/internal/miniyaml"
)

type Policy struct {
	Version     int         `json:"version"`
	VM          VM          `json:"vm"`
	SSH         SSH         `json:"ssh"`
	Files       Files       `json:"files"`
	Network     Network     `json:"network"`
	Clipboard   string      `json:"clipboard"` // off | host-to-guest
	Credentials Credentials `json:"credentials"`
	Monitor     Monitor     `json:"monitor"`
}

// Monitor configures optional guest-side tooling. CC-Monitor (cc-monitor.com)
// audits and guards Claude Code inside the VM. It is a convenience layer: it
// runs in the untrusted guest, so its log is not the system of record.
type Monitor struct {
	CCMonitor bool `json:"cc_monitor"`
}

type VM struct {
	Profile string        `json:"profile"` // headless | desktop
	CPUs    int           `json:"cpus"`
	Memory  int64         `json:"memory"`  // bytes
	Disk    int64         `json:"disk"`    // bytes
	Timeout time.Duration `json:"timeout"` // 0 = unlimited
}

type SSH struct {
	Root bool `json:"root"`
}

type Files struct {
	Mode         string   `json:"mode"`   // copy | none
	Export       string   `json:"export"` // review | auto | deny
	DenyPatterns []string `json:"deny_patterns"`
	MaxImport    int64    `json:"max_import"` // bytes
}

type Network struct {
	Mode      string   `json:"mode"` // allowlist | offline | host (guest shares the host network; loosest)
	Allow     []string `json:"allow"`
	MaxEgress int64    `json:"max_egress"` // bytes, 0 = unlimited
}

type Credentials struct {
	LLMGateway LLMGateway `json:"llm_gateway"`
}

type LLMGateway struct {
	Provider      string `json:"provider"` // anthropic
	KeyRef        string `json:"key_ref"`  // env:NAME | file:PATH | keychain:SERVICE | secret-service:SERVICE
	MonthlyBudget string `json:"monthly_budget"`
}

const (
	KiB = int64(1) << 10
	MiB = KiB << 10
	GiB = MiB << 10
)

// Default is the secure baseline used when no policy file exists.
func Default() Policy {
	return Policy{
		Version: 1,
		VM:      VM{Profile: "desktop", CPUs: 4, Memory: 8 * GiB, Disk: 40 * GiB, Timeout: 8 * time.Hour},
		SSH:     SSH{Root: true},
		Files: Files{
			Mode: "copy", Export: "review",
			DenyPatterns: []string{".env*", "*.pem", "id_*", ".git/config"},
			MaxImport:    500 * MiB,
		},
		Network: Network{
			Mode: "allowlist",
			Allow: []string{
				"archive.ubuntu.com:80", "security.ubuntu.com:80",
				"github.com:443", "pypi.org:443", "files.pythonhosted.org:443", "registry.npmjs.org:443",
				// Claude Code subscription login and API (OAuth authorize/token live on platform.claude.com)
				"claude.ai:443", "platform.claude.com:443", "api.anthropic.com:443",
			},
			MaxEgress: 2 * GiB,
		},
		Clipboard:   "off",
		Monitor:     Monitor{CCMonitor: true},
		Credentials: Credentials{LLMGateway: LLMGateway{Provider: "anthropic"}},
	}
}

// Load reads a policy file on top of Default().
func Load(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	return Parse(string(data))
}

// Parse parses YAML text on top of Default() and validates it.
func Parse(src string) (Policy, error) {
	raw, err := miniyaml.Parse(src)
	if err != nil {
		return Policy{}, err
	}
	p := Default()
	if err := apply(&p, raw); err != nil {
		return Policy{}, err
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func apply(p *Policy, raw map[string]any) error {
	return walk(raw, "", map[string]func(any) error{
		"version":                          func(v any) error { return setInt(v, &p.Version) },
		"vm.profile":                       func(v any) error { return setStr(v, &p.VM.Profile) },
		"vm.cpus":                          func(v any) error { return setInt(v, &p.VM.CPUs) },
		"vm.memory":                        func(v any) error { return setSize(v, &p.VM.Memory) },
		"vm.disk":                          func(v any) error { return setSize(v, &p.VM.Disk) },
		"vm.timeout":                       func(v any) error { return setDur(v, &p.VM.Timeout) },
		"ssh.root":                         func(v any) error { return setBool(v, &p.SSH.Root) },
		"files.mode":                       func(v any) error { return setStr(v, &p.Files.Mode) },
		"files.export":                     func(v any) error { return setStr(v, &p.Files.Export) },
		"files.deny_patterns":              func(v any) error { return setList(v, &p.Files.DenyPatterns) },
		"files.max_import":                 func(v any) error { return setSize(v, &p.Files.MaxImport) },
		"network.mode":                     func(v any) error { return setStr(v, &p.Network.Mode) },
		"network.allow":                    func(v any) error { return setList(v, &p.Network.Allow) },
		"network.max_egress":               func(v any) error { return setSize(v, &p.Network.MaxEgress) },
		"clipboard":                        func(v any) error { return setStr(v, &p.Clipboard) },
		"monitor.cc_monitor":               func(v any) error { return setBool(v, &p.Monitor.CCMonitor) },
		"credentials.llm_gateway.provider": func(v any) error { return setStr(v, &p.Credentials.LLMGateway.Provider) },
		"credentials.llm_gateway.key_ref":  func(v any) error { return setStr(v, &p.Credentials.LLMGateway.KeyRef) },
		"credentials.llm_gateway.monthly_budget": func(v any) error {
			return setStr(v, &p.Credentials.LLMGateway.MonthlyBudget)
		},
	})
}

// walk visits every leaf; unknown paths are errors.
func walk(m map[string]any, prefix string, setters map[string]func(any) error) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sub, ok := m[k].(map[string]any); ok {
			if !hasPrefix(setters, path+".") {
				return fmt.Errorf("unknown policy key %q", path)
			}
			if err := walk(sub, path, setters); err != nil {
				return err
			}
			continue
		}
		set, ok := setters[path]
		if !ok {
			return fmt.Errorf("unknown policy key %q", path)
		}
		if err := set(m[k]); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func hasPrefix(m map[string]func(any) error, pfx string) bool {
	for k := range m {
		if strings.HasPrefix(k, pfx) {
			return true
		}
	}
	return false
}

func setStr(v any, dst *string) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("expected a string")
	}
	*dst = s
	return nil
}

func setBool(v any, dst *bool) error {
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("expected true or false")
	}
	*dst = b
	return nil
}

func setInt[T ~int](v any, dst *T) error {
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("expected an integer")
	}
	*dst = T(n)
	return nil
}

func setList(v any, dst *[]string) error {
	l, ok := v.([]any)
	if !ok {
		if s, isStr := v.(string); isStr && s == "" {
			*dst = nil
			return nil
		}
		return fmt.Errorf("expected a list")
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := e.(string)
		if !ok {
			return fmt.Errorf("list items must be strings")
		}
		out = append(out, s)
	}
	*dst = out
	return nil
}

func setSize(v any, dst *int64) error {
	switch x := v.(type) {
	case int64:
		if x != 0 {
			return fmt.Errorf("sizes need a unit (e.g. 8GiB); only 0 may be bare")
		}
		*dst = 0
		return nil
	case string:
		n, err := ParseSize(x)
		if err != nil {
			return err
		}
		*dst = n
		return nil
	}
	return fmt.Errorf("expected a size like 8GiB")
}

func setDur(v any, dst *time.Duration) error {
	switch x := v.(type) {
	case int64:
		if x != 0 {
			return fmt.Errorf("durations need a unit (e.g. 8h); only 0 may be bare")
		}
		*dst = 0
		return nil
	case string:
		d, err := time.ParseDuration(x)
		if err != nil {
			return err
		}
		if d < 0 {
			return fmt.Errorf("negative duration")
		}
		*dst = d
		return nil
	}
	return fmt.Errorf("expected a duration like 8h")
}

// ParseSize parses "512MiB", "8GiB", "100KiB", "1024B" (binary units only).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{{"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return n * u.mult, nil
		}
	}
	return 0, fmt.Errorf("invalid size %q (use B, KiB, MiB or GiB)", s)
}

func (p Policy) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported policy version %d", p.Version)
	}
	if p.VM.Profile != "headless" && p.VM.Profile != "desktop" {
		return fmt.Errorf("vm.profile must be headless or desktop")
	}
	if p.VM.CPUs < 1 || p.VM.CPUs > 64 {
		return fmt.Errorf("vm.cpus must be between 1 and 64")
	}
	if p.VM.Memory < 512*MiB {
		return fmt.Errorf("vm.memory must be at least 512MiB")
	}
	if p.VM.Disk < 4*GiB {
		return fmt.Errorf("vm.disk must be at least 4GiB")
	}
	if p.Files.Mode != "copy" && p.Files.Mode != "none" {
		return fmt.Errorf("files.mode must be copy or none")
	}
	if p.Files.Export != "review" && p.Files.Export != "auto" && p.Files.Export != "deny" {
		return fmt.Errorf("files.export must be review, auto or deny")
	}
	if p.Files.MaxImport < 0 {
		return fmt.Errorf("files.max_import must not be negative")
	}
	if p.Network.Mode != "allowlist" && p.Network.Mode != "offline" && p.Network.Mode != "host" {
		return fmt.Errorf("network.mode must be allowlist, offline or host")
	}
	for _, a := range p.Network.Allow {
		if err := ValidateAllowEntry(a); err != nil {
			return fmt.Errorf("network.allow %q: %w", a, err)
		}
	}
	if p.Clipboard != "off" && p.Clipboard != "host-to-guest" {
		return fmt.Errorf("clipboard must be off or host-to-guest")
	}
	if p.Credentials.LLMGateway.Provider != "anthropic" {
		return fmt.Errorf("credentials.llm_gateway.provider: only \"anthropic\" is supported")
	}
	return nil
}

// ValidateAllowEntry checks "host:port" where host is a name or "*.suffix".
func ValidateAllowEntry(e string) error {
	host, port, err := net.SplitHostPort(e)
	if err != nil {
		return fmt.Errorf("expected host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port")
	}
	if host == "" || host == "*" || strings.Contains(host, "/") || strings.Contains(host, " ") {
		return fmt.Errorf("invalid host")
	}
	if strings.Contains(host, "*") && !(strings.HasPrefix(host, "*.") && strings.Count(host, "*") == 1 && strings.Count(host[2:], ".") >= 1) {
		return fmt.Errorf("wildcards must look like *.example.com")
	}
	if ip := net.ParseIP(host); ip != nil {
		return fmt.Errorf("IP literals are not allowed; use a domain name")
	}
	return nil
}

// Warnings lists settings that weaken isolation; `glasshouse policy check` prints them.
func (p Policy) Warnings() []string {
	var w []string
	if p.Clipboard != "off" {
		w = append(w, "clipboard is enabled: the guest can read whatever you paste into it")
	}
	if p.Files.Export == "auto" {
		w = append(w, "files.export=auto writes guest changes to your project without review")
	}
	if p.Network.Mode == "host" {
		w = append(w, "network.mode=host: the guest uses the host's network directly. The domain allowlist, the private/metadata address "+
			"blocking and the egress cap do NOT apply; the guest can reach anything the host can, including the host's LAN and the host itself (10.0.2.2 on QEMU, the NAT gateway, usually 192.168.64.1, on macOS)")
	}
	if p.Network.Mode == "allowlist" && p.Network.MaxEgress == 0 {
		w = append(w, "network.max_egress=0 means unlimited outbound bytes")
	}
	for _, a := range p.Network.Allow {
		if strings.HasPrefix(a, "*.") {
			w = append(w, fmt.Sprintf("wildcard allow entry %q covers every subdomain (including user-controlled ones)", a))
		}
		if strings.HasSuffix(a, ":22") {
			w = append(w, fmt.Sprintf("allow entry %q permits raw SSH tunnels through the proxy", a))
		}
	}
	if !p.Monitor.CCMonitor {
		w = append(w, "monitor.cc_monitor is off: no guest-side guard on Claude Code tool calls")
	}
	if len(p.Files.DenyPatterns) == 0 && p.Files.Mode == "copy" {
		w = append(w, "files.deny_patterns is empty: secrets in the project are copied into the guest")
	}
	return w
}

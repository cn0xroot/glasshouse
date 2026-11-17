package policy

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseFull(t *testing.T) {
	p, err := Parse(`
version: 1
vm:
  profile: headless
  cpus: 2
  memory: 4GiB
  disk: 20GiB
  timeout: 2h
files:
  export: deny
network:
  allow: [github.com:443]
  max_egress: 100MiB
credentials:
  llm_gateway:
    key_ref: env:ANTHROPIC_API_KEY
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.VM.Profile != "headless" || p.VM.CPUs != 2 || p.VM.Memory != 4*GiB || p.VM.Timeout != 2*time.Hour {
		t.Fatalf("%+v", p.VM)
	}
	if p.Network.MaxEgress != 100*MiB || len(p.Network.Allow) != 1 {
		t.Fatalf("%+v", p.Network)
	}
	if p.Credentials.LLMGateway.KeyRef != "env:ANTHROPIC_API_KEY" {
		t.Fatal("key_ref")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":    "bogus: 1\n",
		"unknown nested": "vm:\n  gpu: true\n",
		"bad profile":    "vm:\n  profile: weird\n",
		"cpus":           "vm:\n  cpus: 0\n",
		"bare size":      "vm:\n  memory: 8\n",
		"bad unit":       "vm:\n  memory: 8GB\n",
		"wildcard all":   "network:\n  allow: [\"*:443\"]\n",
		"ip literal":     "network:\n  allow: [\"10.0.0.1:80\"]\n",
		"no port":        "network:\n  allow: [github.com]\n",
		"mid wildcard":   "network:\n  allow: [\"a.*.com:443\"]\n",
		"tld wildcard":   "network:\n  allow: [\"*.com:443\"]\n",
		"export":         "files:\n  export: yolo\n",
		"clipboard":      "clipboard: both\n",
		"version":        "version: 2\n",
		"provider":       "credentials:\n  llm_gateway:\n    provider: other\n",
		"scalar for map": "vm: 3\n",
	}
	for name, src := range cases {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestWarnings(t *testing.T) {
	p := Default()
	p.Clipboard = "host-to-guest"
	p.Files.Export = "auto"
	p.Network.Allow = append(p.Network.Allow, "*.github.io:443", "github.com:22")
	w := strings.Join(p.Warnings(), "\n")
	for _, want := range []string{"clipboard", "export=auto", "wildcard", ":22"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning containing %q in:\n%s", want, w)
		}
	}
	if len(Default().Warnings()) != 0 {
		t.Errorf("default policy should have no warnings: %v", Default().Warnings())
	}
}

func TestTightenAllows(t *testing.T) {
	base := Default()
	p, err := Tighten(base, `
vm:
  cpus: 2
  memory: 2GiB
  timeout: 1h
files:
  export: deny
  deny_patterns: ["secrets/*"]
network:
  mode: allowlist
  allow: [github.com:443]
clipboard: off
ssh:
  root: false
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.VM.CPUs != 2 || p.VM.Memory != 2*GiB || p.Files.Export != "deny" || p.SSH.Root {
		t.Fatalf("%+v", p)
	}
	// deny patterns are unioned, not replaced
	joined := strings.Join(p.Files.DenyPatterns, ",")
	if !strings.Contains(joined, ".env*") || !strings.Contains(joined, "secrets/*") {
		t.Fatalf("deny patterns: %v", p.Files.DenyPatterns)
	}
	if len(p.Network.Allow) != 1 || p.Network.Allow[0] != "github.com:443" {
		t.Fatalf("allow: %v", p.Network.Allow)
	}
	// base must be untouched
	if len(base.Network.Allow) < 2 {
		t.Fatal("base mutated")
	}
}

func TestTightenRejectsLoosening(t *testing.T) {
	base := Default()
	base.Clipboard = "off"
	base.Files.Export = "review"
	cases := map[string]string{
		"cpus":       "vm:\n  cpus: 16\n",
		"memory":     "vm:\n  memory: 64GiB\n",
		"timeout0":   "vm:\n  timeout: 0\n",
		"export":     "files:\n  export: auto\n",
		"clipboard":  "clipboard: host-to-guest\n",
		"allow":      "network:\n  allow: [evil.example:443]\n",
		"egress0":    "network:\n  max_egress: 0\n",
		"credential": "credentials:\n  llm_gateway:\n    key_ref: env:X\n",
		"maximport":  "files:\n  max_import: 10GiB\n",
	}
	for name, src := range cases {
		if _, err := Tighten(base, src); err == nil {
			t.Errorf("%s: expected loosening to be rejected", name)
		}
	}
	// offline base cannot be re-opened
	off := Default()
	off.Network.Mode = "offline"
	if _, err := Tighten(off, "network:\n  mode: allowlist\n"); err == nil {
		t.Error("offline -> allowlist should be rejected")
	}
	hl := Default()
	hl.VM.Profile = "headless"
	if _, err := Tighten(hl, "vm:\n  profile: desktop\n"); err == nil {
		t.Error("headless -> desktop should be rejected")
	}
	nf := Default()
	nf.Files.Mode = "none"
	if _, err := Tighten(nf, "files:\n  mode: copy\n"); err == nil {
		t.Error("none -> copy should be rejected")
	}
}

func TestTightenWildcardCoverage(t *testing.T) {
	base := Default()
	base.Network.Allow = []string{"*.example.com:443"}
	if _, err := Tighten(base, "network:\n  allow: [api.example.com:443]\n"); err != nil {
		t.Fatalf("subdomain under wildcard should be allowed: %v", err)
	}
	if _, err := Tighten(base, "network:\n  allow: [example.com:443]\n"); err == nil {
		t.Fatal("apex is not covered by *.example.com")
	}
	if _, err := Tighten(base, "network:\n  allow: [api.example.com:80]\n"); err == nil {
		t.Fatal("port must match")
	}
}

func TestMonitorPolicy(t *testing.T) {
	if !Default().Monitor.CCMonitor {
		t.Fatal("cc_monitor should default to on")
	}
	p, err := Parse("monitor:\n  cc_monitor: false\n")
	if err != nil || p.Monitor.CCMonitor {
		t.Fatalf("%v %+v", err, p.Monitor)
	}
	if w := strings.Join(p.Warnings(), "\n"); !strings.Contains(w, "cc_monitor") {
		t.Errorf("turning the guard off should warn: %s", w)
	}
	// a project may not switch the guard off
	if _, err := Tighten(Default(), "monitor:\n  cc_monitor: false\n"); err == nil {
		t.Error("project policy must not disable the monitor")
	}
	if _, err := Tighten(Default(), "monitor:\n  cc_monitor: true\n"); err != nil {
		t.Errorf("keeping it on is fine: %v", err)
	}
}

func TestNetworkHostModeIsLoosest(t *testing.T) {
	// host is a valid user-policy mode and always produces a warning
	h := Default()
	h.Network.Mode = "host"
	if err := h.Validate(); err != nil {
		t.Fatalf("network.mode host must validate: %v", err)
	}
	warned := false
	for _, w := range h.Warnings() {
		if strings.Contains(w, "network.mode=host") {
			warned = true
		}
	}
	if !warned {
		t.Error("host mode must produce an isolation warning")
	}

	// a project policy can never move to a looser mode...
	for _, c := range []struct{ from, to string }{{"allowlist", "host"}, {"offline", "host"}, {"offline", "allowlist"}} {
		b := Default()
		b.Network.Mode = c.from
		if _, err := Tighten(b, "network:\n  mode: "+c.to+"\n"); err == nil {
			t.Errorf("%s -> %s must be rejected as loosening", c.from, c.to)
		}
	}
	// ...but it can tighten
	for _, c := range []struct{ from, to string }{{"host", "allowlist"}, {"host", "offline"}, {"allowlist", "offline"}, {"host", "host"}} {
		b := Default()
		b.Network.Mode = c.from
		p, err := Tighten(b, "network:\n  mode: "+c.to+"\n")
		if err != nil || p.Network.Mode != c.to {
			t.Errorf("%s -> %s should be allowed: mode=%q err=%v", c.from, c.to, p.Network.Mode, err)
		}
	}
}

func TestDefaultAllowlistHasClaudeLogin(t *testing.T) {
	d := Default()
	for _, want := range []string{"claude.ai:443", "platform.claude.com:443", "api.anthropic.com:443"} {
		found := false
		for _, a := range d.Network.Allow {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("default allowlist is missing %s (needed for subscription login)", want)
		}
	}
}

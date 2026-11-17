package policy

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"glasshouse/internal/miniyaml"
)

// Tighten applies a project policy (.glasshouse.yaml) on top of base. Every value
// must be at least as strict as base; anything looser is an error. Credentials
// cannot be set from a project file.
func Tighten(base Policy, src string) (Policy, error) {
	raw, err := miniyaml.Parse(src)
	if err != nil {
		return Policy{}, err
	}
	p := base
	p.Files.DenyPatterns = append([]string(nil), base.Files.DenyPatterns...)
	p.Network.Allow = append([]string(nil), base.Network.Allow...)

	// Parse into a scratch policy that starts empty-ish so we see what was set.
	scratch := base
	scratch.Files.DenyPatterns = append([]string(nil), base.Files.DenyPatterns...)
	scratch.Network.Allow = append([]string(nil), base.Network.Allow...)
	if err := apply(&scratch, raw); err != nil {
		return Policy{}, err
	}
	set := func(path string) bool { return has(raw, strings.Split(path, ".")) }

	if set("version") && scratch.Version != base.Version {
		return Policy{}, fmt.Errorf("version: cannot differ from the user policy")
	}
	if set("vm.profile") {
		// headless exposes strictly less than desktop
		if base.VM.Profile == "headless" && scratch.VM.Profile == "desktop" {
			return Policy{}, loosen("vm.profile", "desktop", "headless")
		}
		p.VM.Profile = scratch.VM.Profile
	}
	if set("vm.cpus") {
		if scratch.VM.CPUs > base.VM.CPUs {
			return Policy{}, loosen("vm.cpus", scratch.VM.CPUs, base.VM.CPUs)
		}
		p.VM.CPUs = scratch.VM.CPUs
	}
	if set("vm.memory") {
		if scratch.VM.Memory > base.VM.Memory {
			return Policy{}, loosen("vm.memory", scratch.VM.Memory, base.VM.Memory)
		}
		p.VM.Memory = scratch.VM.Memory
	}
	if set("vm.disk") {
		if scratch.VM.Disk > base.VM.Disk {
			return Policy{}, loosen("vm.disk", scratch.VM.Disk, base.VM.Disk)
		}
		p.VM.Disk = scratch.VM.Disk
	}
	if set("vm.timeout") {
		if !durWithin(scratch.VM.Timeout, base.VM.Timeout) {
			return Policy{}, loosen("vm.timeout", scratch.VM.Timeout, base.VM.Timeout)
		}
		p.VM.Timeout = scratch.VM.Timeout
	}
	if set("ssh.root") {
		if scratch.SSH.Root && !base.SSH.Root {
			return Policy{}, loosen("ssh.root", true, false)
		}
		p.SSH.Root = scratch.SSH.Root
	}
	if set("files.mode") {
		if base.Files.Mode == "none" && scratch.Files.Mode == "copy" {
			return Policy{}, loosen("files.mode", "copy", "none")
		}
		p.Files.Mode = scratch.Files.Mode
	}
	if set("files.export") {
		if exportRank(scratch.Files.Export) < exportRank(base.Files.Export) {
			return Policy{}, loosen("files.export", scratch.Files.Export, base.Files.Export)
		}
		p.Files.Export = scratch.Files.Export
	}
	if set("files.deny_patterns") {
		// project patterns are added to, never replace, the user's patterns
		seen := map[string]bool{}
		for _, d := range base.Files.DenyPatterns {
			seen[d] = true
		}
		for _, d := range scratch.Files.DenyPatterns {
			if !seen[d] {
				p.Files.DenyPatterns = append(p.Files.DenyPatterns, d)
				seen[d] = true
			}
		}
	}
	if set("files.max_import") {
		if scratch.Files.MaxImport > base.Files.MaxImport {
			return Policy{}, loosen("files.max_import", scratch.Files.MaxImport, base.Files.MaxImport)
		}
		p.Files.MaxImport = scratch.Files.MaxImport
	}
	if set("network.mode") {
		if networkRank(scratch.Network.Mode) > networkRank(base.Network.Mode) {
			return Policy{}, loosen("network.mode", scratch.Network.Mode, base.Network.Mode)
		}
		p.Network.Mode = scratch.Network.Mode
	}
	if set("network.allow") {
		for _, a := range scratch.Network.Allow {
			if !coveredBy(a, base.Network.Allow) {
				return Policy{}, fmt.Errorf("network.allow: %q is not permitted by the user policy", a)
			}
		}
		p.Network.Allow = scratch.Network.Allow
	}
	if set("network.max_egress") {
		if !egressWithin(scratch.Network.MaxEgress, base.Network.MaxEgress) {
			return Policy{}, loosen("network.max_egress", scratch.Network.MaxEgress, base.Network.MaxEgress)
		}
		p.Network.MaxEgress = scratch.Network.MaxEgress
	}
	if set("clipboard") {
		if base.Clipboard == "off" && scratch.Clipboard != "off" {
			return Policy{}, loosen("clipboard", scratch.Clipboard, "off")
		}
		p.Clipboard = scratch.Clipboard
	}
	if set("monitor.cc_monitor") {
		if base.Monitor.CCMonitor && !scratch.Monitor.CCMonitor {
			return Policy{}, loosen("monitor.cc_monitor", false, true)
		}
		p.Monitor.CCMonitor = scratch.Monitor.CCMonitor
	}
	if set("credentials") {
		return Policy{}, fmt.Errorf("credentials: cannot be set from a project policy")
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// LoadProject reads and tightens a project policy file if it exists.
func LoadProject(base Policy, path string) (Policy, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return base, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	p, err := Tighten(base, string(data))
	if err != nil {
		return Policy{}, true, fmt.Errorf("%s: %w", path, err)
	}
	return p, true, nil
}

func loosen(key string, got, limit any) error {
	return fmt.Errorf("%s: project policy may not loosen the user policy (%v > %v)", key, got, limit)
}

func has(m map[string]any, path []string) bool {
	v, ok := m[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	sub, ok := v.(map[string]any)
	return ok && has(sub, path[1:])
}

func exportRank(s string) int {
	switch s {
	case "deny":
		return 2
	case "review":
		return 1
	}
	return 0
}

// 0 means unlimited, which is looser than any positive value.
func durWithin(v, limit time.Duration) bool {
	if limit == 0 {
		return true
	}
	return v != 0 && v <= limit
}

func egressWithin(v, limit int64) bool {
	if limit == 0 {
		return true
	}
	return v != 0 && v <= limit
}

// coveredBy reports whether entry a is permitted by any base entry
// (exact match, or base wildcard *.x:port covering a subdomain of x).
func coveredBy(a string, base []string) bool {
	sort.Strings(base)
	ah, ap, _ := splitHP(a)
	for _, b := range base {
		bh, bp, _ := splitHP(b)
		if ap != bp {
			continue
		}
		if ah == bh {
			return true
		}
		if strings.HasPrefix(bh, "*.") {
			suffix := bh[1:] // ".example.com"
			if strings.HasSuffix(ah, suffix) && len(ah) > len(suffix) {
				return true
			}
		}
	}
	return false
}

func splitHP(e string) (string, string, bool) {
	i := strings.LastIndex(e, ":")
	if i < 0 {
		return strings.ToLower(e), "", false
	}
	return strings.ToLower(e[:i]), e[i+1:], true
}

// networkRank orders network modes from tightest to loosest: offline < allowlist < host.
func networkRank(mode string) int {
	switch mode {
	case "offline":
		return 0
	case "allowlist":
		return 1
	default: // host, and anything unknown is treated as the loosest
		return 2
	}
}

// glasshouse runs AI agents inside a hardware-virtualized Ubuntu 26.04 VM so they
// never touch the host's files, credentials or network.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"glasshouse/internal/backend"
	"glasshouse/internal/backend/qemu"
	"glasshouse/internal/backend/vz"
	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

const usage = `glasshouse - run AI agents in an isolated Ubuntu 26.04 virtual machine

Usage:
  glasshouse run [flags] [dir] [-- command...]   start a session (copies dir into /workspace)
  glasshouse ssh [--user root|agent] [session] [-- command...]
  glasshouse gui [session]                        open the desktop window
  glasshouse monitor [session]                    open the CC-Monitor web UI (guest guard)
  glasshouse pull [--yes] [--delete] [session] [dir]   bring guest changes back (reviewed)
  glasshouse ls                                   list sessions
  glasshouse logs [--follow|--verify|--console] [session]
  glasshouse start <session> [-- command...]      boot a stopped session again (needs run --keep)
  glasshouse stop [session]
  glasshouse rm <session>
  glasshouse policy check [file]
  glasshouse doctor

run flags:
  --policy FILE   user policy (default ~/.config/glasshouse/policy.yaml, or $GLASSHOUSE_POLICY)
  --gui           desktop mode: GNOME desktop VM, opens the desktop window when ready
  --shell         shell mode: lightweight VM without a desktop, drops into an agent shell in /workspace
  --mode M        same as --gui / --shell (M is gui or shell); without any of them, vm.profile decides
  --headless      alias of --shell
                  (a mode flag never loosens the policy: --gui is refused if the policy forces headless)
  --offline       no outbound network except the model gateway
  --keep          keep the session disk after stop, so glasshouse start <session> can boot it again
  --allow-tcg     allow very slow software emulation when /dev/kvm is missing
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "_supervise":
		err = cmdSupervise(args)
	case "ssh":
		err = cmdSSH(args)
	case "gui":
		err = cmdGUI(args)
	case "monitor":
		err = cmdMonitor(args)
	case "pull":
		err = cmdPull(args)
	case "ls":
		err = cmdLS(args)
	case "logs":
		err = cmdLogs(args)
	case "start":
		err = cmdStart(args)
	case "stop":
		err = cmdStop(args)
	case "rm":
		err = cmdRM(args)
	case "policy":
		err = cmdPolicy(args)
	case "doctor":
		err = cmdDoctor(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "glasshouse:", err)
		os.Exit(1)
	}
}

func hostBackend() (backend.Backend, error) {
	switch runtime.GOOS {
	case "linux":
		return qemu.New(), nil
	case "darwin":
		return vz.New(), nil
	}
	return nil, fmt.Errorf("unsupported host OS %s", runtime.GOOS)
}

// testBackends lets tests register a fake hypervisor; empty in production.
var testBackends = map[string]backend.Backend{}

func backendByName(name string) (backend.Backend, error) {
	if b, ok := testBackends[name]; ok {
		return b, nil
	}
	switch name {
	case "qemu":
		return qemu.New(), nil
	case "vz":
		return vz.New(), nil
	}
	return nil, fmt.Errorf("unknown backend %q", name)
}

func hostArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH, nil
	}
	return "", fmt.Errorf("unsupported CPU architecture %s", runtime.GOARCH)
}

func userPolicyPath(override string) string {
	if override != "" {
		return override
	}
	if p := os.Getenv("GLASSHOUSE_POLICY"); p != "" {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "glasshouse", "policy.yaml")
}

// effectivePolicy = user policy (or secure defaults), tightened by the
// project's .glasshouse.yaml, tightened again by command-line flags.
func effectivePolicy(userFile, project string, headless, offline bool) (policy.Policy, []string, error) {
	var notes []string
	p := policy.Default()
	path := userPolicyPath(userFile)
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			loaded, err := policy.Load(path)
			if err != nil {
				return p, nil, fmt.Errorf("%s: %w", path, err)
			}
			p = loaded
		} else if userFile != "" {
			return p, nil, fmt.Errorf("policy file %s not found", userFile)
		} else {
			notes = append(notes, "no user policy found; using secure defaults")
		}
	}
	if project != "" {
		tp, found, err := policy.LoadProject(p, filepath.Join(project, ".glasshouse.yaml"))
		if err != nil {
			return p, nil, err
		}
		if found {
			notes = append(notes, "applied "+filepath.Join(project, ".glasshouse.yaml")+" (can only tighten)")
		}
		p = tp
	}
	var snippet strings.Builder
	if headless {
		snippet.WriteString("vm:\n  profile: headless\n")
	}
	if offline {
		snippet.WriteString("network:\n  mode: offline\n")
	}
	if snippet.Len() > 0 {
		var err error
		if p, err = policy.Tighten(p, snippet.String()); err != nil {
			return p, nil, err
		}
	}
	return p, notes, nil
}

func sessionHome() (string, error) {
	home, err := session.Home()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	return home, nil
}

// specFromInfo rebuilds the backend spec of an existing session.
func specFromInfo(home string, in session.Info, be backend.Backend) backend.Spec {
	dir := session.Dir(home, in.ID)
	return backend.Spec{
		ID: in.ID, Dir: dir, Arch: in.Arch, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, DiskBytes: in.DiskBytes,
		Base: session.ImagePath(home, in.Arch, in.Profile, be.ImageExt()),
		Disk: filepath.Join(dir, "disk."+be.ImageExt()), Seed: filepath.Join(dir, "seed.iso"),
		GUI: in.GUI, Offline: in.Offline, HostNet: in.HostNet, SSHPort: in.SSHPort,
		ProxyAddr: in.ProxyAddr, GatewayAddr: in.GatewayAddr, AllowTCG: in.AllowTCG,
	}
}

func loadSessionPolicy(home, id string) (policy.Policy, error) {
	return readPolicySnapshot(filepath.Join(session.Dir(home, id), "policy.json"))
}

func sshArgv(home string, in session.Info, user string) ([]string, error) {
	be, err := backendByName(in.Backend)
	if err != nil {
		return nil, err
	}
	spec := specFromInfo(home, in, be)
	return be.SSHArgs(spec, user, filepath.Join(spec.Dir, "id_ed25519")), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func lookOrErr(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found in PATH", name)
	}
	return p, nil
}

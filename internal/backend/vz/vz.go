// Package vz is the macOS backend. It drives Apple's Virtualization.framework
// through the glasshouse-vz helper (see vz-helper/), which is a separate signed
// process with the virtualization entitlement and, ideally, no network access.
//
// The guest has NO network device. It reaches the host only through vsock:
// sshd (host connects in), plus loopback bridges inside the guest to the
// egress proxy and the model gateway (host-side unix sockets). That keeps the
// App-Sandboxed helper off the network entirely.
package vz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"glasshouse/internal/backend"
)

type Backend struct{ Helper string }

func New() *Backend {
	h := os.Getenv("GLASSHOUSE_VZ")
	if h == "" {
		if exe, err := os.Executable(); err == nil {
			h = filepath.Join(filepath.Dir(exe), "glasshouse-vz")
		}
	}
	return &Backend{Helper: h}
}

func (b *Backend) Name() string     { return "vz" }
func (b *Backend) ImageExt() string { return "raw" }

func (b *Backend) Preflight(spec *backend.Spec) error {
	if runtime.GOOS != "darwin" {
		return errors.New("the vz backend only runs on macOS")
	}
	if spec.Arch != runtime.GOARCH {
		return fmt.Errorf("guest architecture %s does not match this Mac (%s): Virtualization.framework cannot emulate", spec.Arch, runtime.GOARCH)
	}
	if translated() {
		return errors.New("glasshouse is running under Rosetta (an amd64 build on Apple silicon): rebuild it natively with `make build` (GOARCH=arm64) so it boots arm64 guests")
	}
	if st, err := os.Stat(b.Helper); err != nil || st.IsDir() {
		return fmt.Errorf("glasshouse-vz helper not found at %s (build it with `make vz-helper`, or set GLASSHOUSE_VZ)", b.Helper)
	}
	if err := checkEntitlement(b.Helper); err != nil {
		return err
	}
	hc, err := b.Check()
	if err != nil {
		return err
	}
	if !hc.Supported {
		return errors.New("Virtualization.framework reports this Mac cannot run VMs (needs macOS 13+; not available inside another VM)")
	}
	if hc.Arch != "" && hc.Arch != runtime.GOARCH {
		return fmt.Errorf("glasshouse-vz is a %s binary but glasshouse is %s: rebuild both natively (make build vz-helper)", hc.Arch, runtime.GOARCH)
	}
	// The helper clamps too; refusing here gives a clear message instead of a silently smaller VM.
	if spec.CPUs > hc.MaxCPUs && hc.MaxCPUs > 0 {
		return fmt.Errorf("vm.cpus=%d exceeds what this Mac allows (%d)", spec.CPUs, hc.MaxCPUs)
	}
	if hc.MaxMemoryMiB > 0 && spec.MemoryMiB > hc.MaxMemoryMiB {
		return fmt.Errorf("vm.memory=%dMiB exceeds what this Mac allows (%dMiB)", spec.MemoryMiB, hc.MaxMemoryMiB)
	}
	if spec.Dir != "" {
		if err := checkAPFS(filepath.Dir(spec.Dir)); err != nil {
			return err
		}
	}
	return nil
}

// HostCheck is the output of `glasshouse-vz check`.
type HostCheck struct {
	Supported    bool   `json:"supported"`
	Arch         string `json:"arch"`
	MinCPUs      int    `json:"minCPUs"`
	MaxCPUs      int    `json:"maxCPUs"`
	MinMemoryMiB int    `json:"minMemoryMiB"`
	MaxMemoryMiB int    `json:"maxMemoryMiB"`
}

// Check runs the helper's self-test, which also proves it starts at all (signature, architecture, macOS version).
func (b *Backend) Check() (HostCheck, error) {
	var hc HostCheck
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, b.Helper, "check").Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return hc, fmt.Errorf("glasshouse-vz does not run (%s); rebuild it with `make vz-helper`", msg)
	}
	if err := json.Unmarshal(out, &hc); err != nil {
		return hc, fmt.Errorf("glasshouse-vz is too old (no `check` command); rebuild it with `make vz-helper`")
	}
	return hc, nil
}

// checkEntitlement makes sure the helper is signed with com.apple.security.virtualization. Without it the VM
// fails to start with an opaque "Invalid virtual machine configuration" error.
func checkEntitlement(helper string) error {
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", helper).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "com.apple.security.virtualization") {
		return fmt.Errorf("%s is not signed with the virtualization entitlement; run: codesign --force --sign - --entitlements vz-helper/glasshouse-vz.entitlements %s (or `make vz-helper`)", helper, helper)
	}
	return nil
}

// PrepareDisk clones the raw template with APFS copy-on-write (cp -c) and
// grows the clone to the requested size.
func (b *Backend) PrepareDisk(spec *backend.Spec) error {
	if out, err := exec.Command("cp", "-c", spec.Base, spec.Disk).CombinedOutput(); err != nil {
		return fmt.Errorf("clone base image (needs an APFS volume): %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(spec.Disk, 0o600); err != nil {
		return err
	}
	st, err := os.Stat(spec.Disk)
	if err != nil {
		return err
	}
	if spec.DiskBytes > st.Size() {
		return os.Truncate(spec.Disk, spec.DiskBytes)
	}
	return nil
}

func (b *Backend) NetdEndpoints(spec backend.Spec) (backend.Endpoint, backend.Endpoint) {
	return backend.Endpoint{Network: "unix", Addr: filepath.Join(spec.Dir, "proxy.sock")},
		backend.Endpoint{Network: "unix", Addr: filepath.Join(spec.Dir, "gateway.sock")}
}

// GuestNet: SSH and the model gateway always go over vsock. The egress proxy is used in allowlist mode only; in
// network.mode=host the guest has a NAT NIC and reaches the network itself.
func (b *Backend) GuestNet(spec backend.Spec) backend.GuestNet {
	g := backend.GuestNet{GatewayURL: "http://127.0.0.1:8080", VsockBridge: true}
	if !spec.Offline && !spec.HostNet {
		g.ProxyHost, g.ProxyPort = "127.0.0.1", 3128
	}
	return g
}

type forward struct {
	VsockPort uint32 `json:"vsockPort"`
	Unix      string `json:"unix"`
}

// Config is the contract with vz-helper/Sources/glasshouse-vz/main.swift.
type Config struct {
	CPUs      int       `json:"cpus"`
	MemoryMiB int       `json:"memoryMiB"`
	Disk      string    `json:"disk"`
	Seed      string    `json:"seed"`
	GUI       bool      `json:"gui"`
	SSHSock   string    `json:"sshSock"`  // host unix socket -> guest vsock :22
	CtrlSock  string    `json:"ctrlSock"` // "show" / "stop" commands
	EFIVars   string    `json:"efiVars"`
	ReadyFile string    `json:"readyFile"`
	Forwards  []forward `json:"forwards"` // guest vsock port -> host unix socket
	// NAT gives the guest a virtio NIC on Virtualization.framework's NAT network: it then uses the Mac's network
	// (routing, DNS, VPN/TUN) directly. Only network.mode=host sets it; every other mode leaves the guest without a NIC.
	NAT bool `json:"nat,omitempty"`
	// The helper also accepts "extraDisks" (read-only build inputs) for image/build-mac.sh; sessions never use it.
}

func BuildConfig(spec backend.Spec) Config {
	c := Config{
		CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB, Disk: spec.Disk, Seed: spec.Seed, GUI: spec.GUI,
		SSHSock:   filepath.Join(spec.Dir, "ssh.sock"),
		CtrlSock:  filepath.Join(spec.Dir, "ctrl.sock"),
		EFIVars:   filepath.Join(spec.Dir, "efi-vars"),
		ReadyFile: filepath.Join(spec.Dir, "vz.ready"),
		Forwards:  []forward{{VsockPort: 8080, Unix: spec.GatewayAddr}},
	}
	switch {
	case spec.HostNet:
		c.NAT = true // the guest's own NIC; no egress proxy, no allowlist
	case !spec.Offline:
		c.Forwards = append(c.Forwards, forward{VsockPort: 3128, Unix: spec.ProxyAddr})
	}
	return c
}

func (b *Backend) Start(ctx context.Context, spec backend.Spec) (backend.Process, error) {
	cfg := BuildConfig(spec)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(spec.Dir, "vz.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(spec.Dir, "vz.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(b.Helper, "run", "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); logf.Close(); close(p.done) }()
	return p, nil
}

// process: Wait may be called from several goroutines at once (the supervisor's exit watcher and Stop). The result
// is published by closing done, so every waiter returns; a single-value channel would leave all but one blocked.
type process struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (p *process) PID() int { return p.cmd.Process.Pid }
func (p *process) Wait() error {
	<-p.done
	return p.err
}

// stopGrace is the least time the helper gets after SIGTERM. It asks the guest to power off and forces the VM off
// itself after 10 s (main.swift: forcedStopAfter); SIGKILLing it before that would leave the VM running in the
// Virtualization XPC process with nothing left to stop it.
const stopGrace = 20 * time.Second

func (p *process) Stop(grace time.Duration) error {
	if grace < stopGrace {
		grace = stopGrace
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM) // helper asks the guest to power off
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

func (b *Backend) SSHArgs(spec backend.Spec, user, keyPath string) []string {
	sock := filepath.Join(spec.Dir, "ssh.sock")
	args := append([]string{"ssh"}, backend.BaseSSHOptions(keyPath)...)
	return append(args, "-o", "ProxyCommand=nc -U '"+sock+"'", user+"@glasshouse")
}

func (b *Backend) OpenDisplay(spec backend.Spec) error {
	if !spec.GUI {
		return errors.New("this session was started without a GUI (profile headless)")
	}
	c, err := net.DialTimeout("unix", filepath.Join(spec.Dir, "ctrl.sock"), 2*time.Second)
	if err != nil {
		return errors.New("cannot reach the VM helper; is the session running?")
	}
	defer c.Close()
	_, err = c.Write([]byte("show\n"))
	return err
}

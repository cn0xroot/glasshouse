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
	if spec.HostNet {
		return errors.New("network.mode=host is not supported by the macOS backend yet (the VM has no NIC; all traffic goes over vsock)")
	}
	if spec.Arch != runtime.GOARCH {
		return fmt.Errorf("guest architecture %s does not match this Mac (%s): Virtualization.framework cannot emulate", spec.Arch, runtime.GOARCH)
	}
	if st, err := os.Stat(b.Helper); err != nil || st.IsDir() {
		return fmt.Errorf("glasshouse-vz helper not found at %s (build it with `make vz-helper`, or set GLASSHOUSE_VZ)", b.Helper)
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

func (b *Backend) GuestNet(spec backend.Spec) backend.GuestNet {
	g := backend.GuestNet{GatewayURL: "http://127.0.0.1:8080", VsockBridge: true}
	if !spec.Offline {
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
	if !spec.Offline {
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
	p := &process{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); logf.Close() }()
	return p, nil
}

type process struct {
	cmd  *exec.Cmd
	done chan error
	err  error
	got  bool
}

func (p *process) PID() int { return p.cmd.Process.Pid }
func (p *process) Wait() error {
	if !p.got {
		p.err, p.got = <-p.done, true
	}
	return p.err
}
func (p *process) Stop(grace time.Duration) error {
	waited := make(chan struct{})
	go func() { _ = p.Wait(); close(waited) }()
	_ = p.cmd.Process.Signal(syscall.SIGTERM) // helper asks the guest to power off
	select {
	case <-waited:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-waited
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

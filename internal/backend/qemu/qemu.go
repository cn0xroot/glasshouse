// Package qemu is the Linux backend: QEMU with KVM, run as the ordinary user,
// seccomp-sandboxed, with only virtio devices and a user-mode network that is
// restricted to three explicit forwards (ssh in, proxy out, gateway out).
package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"glasshouse/internal/backend"
)

const (
	GuestProxyIP   = "10.0.2.100"
	GuestProxyPort = 3128
	GuestGWIP      = "10.0.2.101"
	GuestGWPort    = 8080
)

type Backend struct {
	// Overridable for tests.
	LookPath  func(string) (string, error)
	KVMExists func() bool
}

func New() *Backend {
	return &Backend{
		LookPath: exec.LookPath,
		KVMExists: func() bool {
			f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
			if err != nil {
				return false
			}
			f.Close()
			return true
		},
	}
}

func (b *Backend) Name() string { return "qemu" }

func binary(arch string) (string, error) {
	switch arch {
	case "amd64":
		return "qemu-system-x86_64", nil
	case "arm64":
		return "qemu-system-aarch64", nil
	}
	return "", fmt.Errorf("unsupported architecture %q", arch)
}

var uefiCandidates = [][2]string{
	{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
	{"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd", ""},
}

func (b *Backend) Preflight(spec *backend.Spec) error {
	bin, err := binary(spec.Arch)
	if err != nil {
		return err
	}
	if _, err := b.LookPath(bin); err != nil {
		return fmt.Errorf("%s not found (install qemu-system-x86 / qemu-system-arm)", bin)
	}
	if _, err := b.LookPath("qemu-img"); err != nil {
		return errors.New("qemu-img not found (install qemu-utils)")
	}
	if !b.KVMExists() && !spec.AllowTCG {
		return errors.New("/dev/kvm is not accessible: enable virtualization in firmware and add your user to the kvm group (or pass --allow-tcg for very slow emulation)")
	}
	if spec.Arch == "arm64" {
		for _, c := range uefiCandidates {
			if _, err := os.Stat(c[0]); err == nil {
				spec.UEFICode, spec.UEFIVars = c[0], c[1]
				return nil
			}
		}
		return errors.New("arm64 UEFI firmware not found (install qemu-efi-aarch64)")
	}
	return nil
}

func (b *Backend) ImageExt() string { return "qcow2" }

// PrepareDisk makes a qcow2 overlay whose backing file is the read-only
// template, so discarding the session discards every change the guest made.
func (b *Backend) PrepareDisk(spec *backend.Spec) error {
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2",
		"-b", spec.Base, "-F", "qcow2", spec.Disk, strconv.FormatInt(spec.DiskBytes, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(spec.Disk, 0o600); err != nil {
		return err
	}
	if spec.Arch == "arm64" && spec.UEFIVars != "" {
		data, err := os.ReadFile(spec.UEFIVars)
		if err != nil {
			return err
		}
		dst := filepath.Join(spec.Dir, "vars.fd")
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
		spec.UEFIVars = dst
	}
	return nil
}

func (b *Backend) NetdEndpoints(spec backend.Spec) (backend.Endpoint, backend.Endpoint) {
	return backend.Endpoint{Network: "tcp", Addr: "127.0.0.1:0"}, backend.Endpoint{Network: "tcp", Addr: "127.0.0.1:0"}
}

func (b *Backend) GuestNet(spec backend.Spec) backend.GuestNet {
	g := backend.GuestNet{GatewayURL: fmt.Sprintf("http://%s:%d", GuestGWIP, GuestGWPort)}
	if !spec.Offline && !spec.HostNet {
		g.ProxyHost, g.ProxyPort = GuestProxyIP, GuestProxyPort
	}
	return g
}

func hostPort(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(p)
}

// Args builds the QEMU command line. It is a pure function so the security
// relevant flags can be unit-tested without a hypervisor.
func Args(spec backend.Spec, useKVM bool) ([]string, error) {
	if spec.SSHPort <= 0 {
		return nil, errors.New("ssh port not allocated")
	}
	gwPort, err := hostPort(spec.GatewayAddr)
	if err != nil {
		return nil, fmt.Errorf("gateway address: %w", err)
	}
	// Default: restrict=on cuts the guest off from the host network except for the explicit forwards below.
	// network.mode=host: plain user-mode NAT, so the guest uses the host's network (and DNS) directly.
	net0 := fmt.Sprintf("user,id=n0,restrict=on,hostfwd=tcp:127.0.0.1:%d-:22", spec.SSHPort)
	if spec.HostNet {
		net0 = fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", spec.SSHPort)
	}
	if !spec.Offline && !spec.HostNet {
		pp, err := hostPort(spec.ProxyAddr)
		if err != nil {
			return nil, fmt.Errorf("proxy address: %w", err)
		}
		net0 += fmt.Sprintf(",guestfwd=tcp:%s:%d-tcp:127.0.0.1:%d", GuestProxyIP, GuestProxyPort, pp)
	}
	net0 += fmt.Sprintf(",guestfwd=tcp:%s:%d-tcp:127.0.0.1:%d", GuestGWIP, GuestGWPort, gwPort)

	var machine, cpu string
	switch spec.Arch {
	case "amd64":
		machine = "q35"
	case "arm64":
		machine = "virt"
	default:
		return nil, fmt.Errorf("unsupported architecture %q", spec.Arch)
	}
	if useKVM {
		machine += ",accel=kvm"
		cpu = "host"
	} else {
		machine += ",accel=tcg"
		cpu = "max"
	}

	a := []string{
		"-name", "glasshouse-" + spec.ID,
		"-machine", machine,
		"-cpu", cpu,
		"-smp", strconv.Itoa(spec.CPUs),
		"-m", strconv.Itoa(spec.MemoryMiB),
		// Reduce what a compromised QEMU can do to the host.
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-nodefaults", "-no-user-config",
		"-rtc", "base=utc,clock=host",
		"-pidfile", filepath.Join(spec.Dir, "qemu.pid"),
		"-monitor", "none",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", filepath.Join(spec.Dir, "qmp.sock")),
		"-serial", "file:" + filepath.Join(spec.Dir, "console.log"),
	}
	if spec.Arch == "arm64" {
		if spec.UEFICode == "" {
			return nil, errors.New("arm64 needs UEFI firmware")
		}
		a = append(a, "-drive", "if=pflash,format=raw,readonly=on,file="+spec.UEFICode)
		if spec.UEFIVars != "" {
			a = append(a, "-drive", "if=pflash,format=raw,file="+spec.UEFIVars)
		}
	}
	a = append(a,
		"-drive", "if=none,id=root,file="+spec.Disk+",format=qcow2,discard=unmap",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", "if=none,id=seed,file="+spec.Seed+",format=raw,media=cdrom,readonly=on",
		"-device", "scsi-cd,drive=seed,bus=scsi0.0",
		"-netdev", net0,
		"-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
	)
	if spec.GUI {
		// Software-rendered 2D display only: no virgl/venus, no host GPU access.
		a = append(a,
			"-device", "virtio-gpu-pci",
			"-device", "qemu-xhci,id=xhci",
			"-device", "usb-kbd,bus=xhci.0",
			"-device", "usb-tablet,bus=xhci.0",
			"-display", "none",
			"-vnc", "unix:"+filepath.Join(spec.Dir, "vnc.sock"),
		)
	} else {
		a = append(a, "-display", "none")
	}
	return a, nil
}

func (b *Backend) Start(ctx context.Context, spec backend.Spec) (backend.Process, error) {
	bin, err := binary(spec.Arch)
	if err != nil {
		return nil, err
	}
	args, err := Args(spec, b.KVMExists())
	if err != nil {
		return nil, err
	}
	argv := append([]string{bin}, args...)
	// Put QEMU in its own cgroup scope with hard CPU/memory ceilings when the
	// user systemd manager is available.
	if _, err := b.LookPath("systemd-run"); err == nil && os.Getenv("XDG_RUNTIME_DIR") != "" {
		wrap := []string{"systemd-run", "--user", "--scope", "--quiet",
			"-p", fmt.Sprintf("MemoryMax=%dM", spec.MemoryMiB+1024),
			"-p", fmt.Sprintf("CPUQuota=%d%%", spec.CPUs*100),
			"-p", "TasksMax=512", "--"}
		argv = append(wrap, argv...)
	}
	logf, err := os.OpenFile(filepath.Join(spec.Dir, "qemu.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	p := &process{cmd: cmd, qmp: filepath.Join(spec.Dir, "qmp.sock"), done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); logf.Close(); close(p.done) }()
	return p, nil
}

type process struct {
	cmd  *exec.Cmd
	qmp  string
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
	_ = qmpPowerdown(p.qmp)
	select {
	case <-waited:
		return nil
	case <-time.After(grace):
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
	<-waited
	return nil
}

func qmpPowerdown(sock string) error {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil { // greeting
		return err
	}
	enc := json.NewEncoder(c)
	for _, cmd := range []string{"qmp_capabilities", "system_powerdown"} {
		if err := enc.Encode(map[string]string{"execute": cmd}); err != nil {
			return err
		}
		if _, err := r.ReadString('\n'); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) SSHArgs(spec backend.Spec, user, keyPath string) []string {
	args := append([]string{"ssh"}, backend.BaseSSHOptions(keyPath)...)
	return append(args, "-p", strconv.Itoa(spec.SSHPort), user+"@127.0.0.1")
}

// OpenDisplay opens the VNC socket in a viewer that understands unix sockets
// (virt-viewer). Without one we refuse rather than expose an unauthenticated
// VNC server on a TCP port.
func (b *Backend) OpenDisplay(spec backend.Spec) error {
	if !spec.GUI {
		return errors.New("this session was started without a GUI (profile headless)")
	}
	sock := filepath.Join(spec.Dir, "vnc.sock")
	if _, err := os.Stat(sock); err != nil {
		return errors.New("display socket not found; is the session running?")
	}
	viewer, err := b.LookPath("remote-viewer")
	if err != nil {
		return errors.New("remote-viewer not found: install virt-viewer (and socat), then run: glasshouse gui " + spec.ID)
	}
	port, err := b.vncBridge(sock)
	if err != nil {
		return fmt.Errorf("%w\n  by hand: socat TCP-LISTEN:5901,bind=127.0.0.1,reuseaddr UNIX-CONNECT:%s & remote-viewer vnc://127.0.0.1:5901", err, sock)
	}
	cmd := exec.Command(viewer, fmt.Sprintf("vnc://127.0.0.1:%d", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// vncBridge makes the display's unix socket reachable over loopback TCP, because virt-viewer 11 cannot open
// vnc+unix:// ("vnc_display_open_host: assertion 'host != NULL' failed"). socat accepts exactly one connection on
// 127.0.0.1 and exits when it ends, and a helper closes it after a minute if nobody connects, so the port is open only briefly
// (the display has no password: while it is open, any local user could connect).
func (b *Backend) vncBridge(sock string) (int, error) {
	socat, err := b.LookPath("socat")
	if err != nil {
		return 0, errors.New("socat not found: install socat (virt-viewer cannot open the display's unix socket directly)")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(socat, fmt.Sprintf("TCP-LISTEN:%d,bind=127.0.0.1,reuseaddr", port), "UNIX-CONNECT:"+sock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait()
	// socat closes its listening socket once it has accepted the connection. If the port is still listening after a
	// minute nobody came, so close the bridge (a detached helper, since this process may be gone by then). A viewer that
	// is connected is not touched.
	watch := fmt.Sprintf("sleep 60; grep -q ':%04X 00000000:0000 0A' /proc/net/tcp && kill %d", port, cmd.Process.Pid)
	wcmd := exec.Command("sh", "-c", watch)
	wcmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = wcmd.Start()
	if wcmd.Process != nil {
		go wcmd.Wait()
	}
	// no probe connection: socat takes only one, and that one must be the viewer's. Wait until the port is listening.
	for i := 0; i < 50; i++ {
		if listening(port) {
			return port, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, errors.New("the display bridge (socat) did not start")
}

// listening reports whether a local TCP port is in the LISTEN state, read from /proc without connecting to it.
func listening(port int) bool {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false
	}
	want := fmt.Sprintf(":%04X 00000000:0000 0A", port)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

var _ = strings.TrimSpace

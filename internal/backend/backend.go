// Package backend hides the hypervisor behind one interface so the policy
// engine, network gate and file bridge never know whether QEMU or Apple's
// Virtualization.framework is underneath.
package backend

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// Spec is the platform-neutral description of one VM.
type Spec struct {
	ID        string
	Dir       string // session directory (private to the host user)
	Arch      string // amd64 | arm64
	CPUs      int
	MemoryMiB int
	DiskBytes int64
	Base      string // read-only template image
	Disk      string // per-session copy-on-write overlay
	Seed      string // cloud-init NoCloud ISO
	GUI       bool
	Offline   bool // no outbound proxy at all; only the model gateway is reachable
	HostNet   bool // guest shares the host network (NAT); no proxy, no allowlist, no restrict

	// Filled in by the supervisor once netd is listening.
	ProxyAddr   string // tcp "127.0.0.1:port" (qemu) or unix path (vz)
	GatewayAddr string
	SSHPort     int // qemu: loopback port forwarded to guest sshd

	AllowTCG bool // permit slow software emulation when KVM is unavailable

	UEFICode string // arm64 firmware (discovered by Preflight)
	UEFIVars string // per-session writable copy of the varstore
}

// GuestNet tells cloud-init how the guest reaches the host-side services.
type GuestNet struct {
	ProxyHost   string // address the guest uses for http(s)_proxy ("" when Offline or HostNet)
	ProxyPort   int
	GatewayURL  string // ANTHROPIC_BASE_URL inside the guest
	VsockBridge bool   // guest must run socat bridges between loopback ports and vsock
}

// Endpoint is where the supervisor must make netd listen.
type Endpoint struct {
	Network string // "tcp" | "unix"
	Addr    string
}

type Process interface {
	PID() int
	Wait() error // blocks until the VM exits
	// Stop asks the guest to power off and kills it after grace.
	Stop(grace time.Duration) error
}

type Backend interface {
	Name() string
	// Preflight checks the host (binaries, /dev/kvm, firmware) and may fill spec.
	Preflight(spec *Spec) error
	// ImageExt is the file extension of base images this backend boots.
	ImageExt() string
	// PrepareDisk creates the per-session copy-on-write disk at spec.Disk.
	PrepareDisk(spec *Spec) error
	NetdEndpoints(spec Spec) (proxy, gateway Endpoint)
	GuestNet(spec Spec) GuestNet
	Start(ctx context.Context, spec Spec) (Process, error)
	// SSHArgs returns the full ssh argv (without a trailing command).
	SSHArgs(spec Spec, user, keyPath string) []string
	OpenDisplay(spec Spec) error
}

// WaitSSH polls until the guest's sshd answers with a protocol banner.
func WaitSSH(ctx context.Context, dial func(context.Context) (net.Conn, error)) error {
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return errors.Join(ctx.Err(), last)
			}
			return ctx.Err()
		default:
		}
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		c, err := dial(dctx)
		cancel()
		if err == nil {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, rerr := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if rerr == nil && strings.HasPrefix(line, "SSH-") {
				return nil
			}
			last = rerr
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// Common SSH client options. The guest's host key changes every session and
// the path is a private loopback/unix channel, so host-key pinning adds nothing.
func BaseSSHOptions(keyPath string) []string {
	return []string{
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=15",
	}
}

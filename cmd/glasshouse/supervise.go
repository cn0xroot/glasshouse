package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"glasshouse/internal/audit"
	"glasshouse/internal/backend"
	"glasshouse/internal/netd"
	"glasshouse/internal/secrets"
	"glasshouse/internal/session"
)

const llmUpstream = "https://api.anthropic.com"

// cmdSupervise is the long-running per-session process. It owns the audit log,
// the network gate and the hypervisor, and cleans up when the VM ends.
func cmdSupervise(args []string) (err error) {
	if len(args) != 1 {
		return errors.New("usage: glasshouse _supervise <session>")
	}
	id := args[0]
	home, err := session.Home()
	if err != nil {
		return err
	}
	info, err := session.Load(home, id)
	if err != nil {
		return err
	}
	dir := session.Dir(home, id)

	fail := func(err error) error {
		info.State, info.Error = "failed", err.Error()
		_ = session.Save(home, info)
		return err
	}
	lg, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		return fail(fmt.Errorf("audit log: %w", err)) // fail closed
	}
	defer lg.Close()
	failLog := func(err error) error {
		_ = lg.Write("session.failed", map[string]any{"error": err.Error()})
		return fail(err)
	}

	info.Supervisor, info.State = os.Getpid(), "starting"
	if err := session.Save(home, info); err != nil {
		return err
	}
	pol, err := loadSessionPolicy(home, id)
	if err != nil {
		return failLog(err)
	}
	be, err := backendByName(info.Backend)
	if err != nil {
		return failLog(err)
	}
	spec := specFromInfo(home, info, be)
	if err := be.Preflight(&spec); err != nil {
		return failLog(err)
	}
	if v := filepath.Join(dir, "vars.fd"); spec.Arch == "arm64" && be.Name() == "qemu" {
		if _, err := os.Stat(v); err == nil {
			spec.UEFIVars = v
		}
	}

	// ---- network gate ----
	allow := pol.Network.Allow
	if info.Offline {
		allow = nil
	}
	nd, err := netd.New(netd.Config{Allow: allow, MaxEgress: pol.Network.MaxEgress, Audit: lg})
	if err != nil {
		return failLog(err)
	}
	proxyEp, gwEp := be.NetdEndpoints(spec)
	gwL, err := listenEndpoint(gwEp)
	if err != nil {
		return failLog(err)
	}
	defer gwL.Close()
	spec.GatewayAddr = addrOf(gwEp, gwL)
	info.GatewayAddr = spec.GatewayAddr
	var proxyL net.Listener
	if !info.Offline && !info.HostNet {
		if proxyL, err = listenEndpoint(proxyEp); err != nil {
			return failLog(err)
		}
		defer proxyL.Close()
		spec.ProxyAddr = addrOf(proxyEp, proxyL)
		info.ProxyAddr = spec.ProxyAddr
		go netd.NewProxy(nd, info.Token).Serve(proxyL)
	}
	keyFn := cachedKey(pol.Credentials.LLMGateway.KeyRef)
	gw, err := netd.NewGateway(llmUpstream, info.Token, keyFn, lg)
	if err != nil {
		return failLog(err)
	}
	go gw.Serve(gwL)

	if be.Name() == "qemu" {
		if spec.SSHPort, err = freePort(); err != nil {
			return failLog(err)
		}
		info.SSHPort = spec.SSHPort
	}
	if err := session.Save(home, info); err != nil {
		return failLog(err)
	}

	// ---- audit socket for CLI-side events ----
	auditL, err := net.Listen("unix", filepath.Join(dir, "audit.sock"))
	if err != nil {
		return failLog(err)
	}
	defer auditL.Close()
	go serveAudit(auditL, lg)

	if err := lg.Write("session.start", map[string]any{
		"id": id, "backend": be.Name(), "profile": info.Profile, "project": info.Project,
		"cpus": info.CPUs, "memory_mib": info.MemoryMiB, "offline": info.Offline, "host_net": info.HostNet,
		"cc_monitor": info.CCMonitor, "root_ssh": info.RootSSH, "boot": info.Boots,
		"allow": allow, "export": pol.Files.Export, "max_egress": pol.Network.MaxEgress,
	}); err != nil {
		return fail(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc, err := be.Start(ctx, spec)
	if err != nil {
		return failLog(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()

	// ---- wait for sshd ----
	dial := func(c context.Context) (net.Conn, error) {
		var d net.Dialer
		if be.Name() == "qemu" {
			return d.DialContext(c, "tcp", fmt.Sprintf("127.0.0.1:%d", spec.SSHPort))
		}
		return d.DialContext(c, "unix", filepath.Join(dir, "ssh.sock"))
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, 5*time.Minute)
	go func() {
		select {
		case <-exited: // VM died during boot
			bootCancel()
		case <-bootCtx.Done():
		}
	}()
	if err := backend.WaitSSH(bootCtx, dial); err != nil {
		bootCancel()
		_ = proc.Stop(5 * time.Second)
		return failLog(fmt.Errorf("guest did not become reachable: %w (see console.log)", err))
	}
	bootCancel()
	info.State = "ready"
	if err := session.Save(home, info); err != nil {
		return failLog(err)
	}
	_ = lg.Write("session.ready", nil)

	// ---- run until the VM exits, we are told to stop, or the time limit hits ----
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	var limit <-chan time.Time
	if pol.VM.Timeout > 0 {
		limit = time.After(pol.VM.Timeout)
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	reason := "vm-exit"
loop:
	for {
		select {
		case <-exited:
			break loop
		case s := <-sig:
			reason = "signal-" + s.String()
			break loop
		case <-limit:
			reason = "timeout"
			break loop
		case <-tick.C:
			_ = session.SaveStats(home, id, session.Stats{
				Egress: nd.Egress(), Ingress: nd.Ingress(), LLMRequests: gw.Requests(), Updated: time.Now().UTC(),
			})
		}
	}

	info.State = "stopping"
	_ = session.Save(home, info)
	select {
	case <-exited:
	default:
		_ = proc.Stop(30 * time.Second)
	}
	st := session.Stats{Egress: nd.Egress(), Ingress: nd.Ingress(), LLMRequests: gw.Requests(), Updated: time.Now().UTC()}
	_ = session.SaveStats(home, id, st)
	_ = lg.Write("session.stop", map[string]any{
		"reason": reason, "egress": st.Egress, "ingress": st.Ingress, "llm_requests": st.LLMRequests,
	})
	cleanup(dir, info.Persistent)
	info.State, info.Token = "stopped", ""
	return session.Save(home, info)
}

// cleanup removes everything the guest could have changed or that holds
// per-session secrets. The audit log and session record stay.
func cleanup(dir string, keepDisk bool) {
	rm := []string{"id_ed25519", "id_ed25519.pub", "seed.iso", "qmp.sock", "vnc.sock", "audit.sock",
		"ssh.sock", "ctrl.sock", "proxy.sock", "gateway.sock", "monitor.sock", "vz.json", "vz.ready", "qemu.pid"}
	if !keepDisk {
		// the UEFI variable store belongs to the disk: a kept disk needs it to boot again
		rm = append(rm, "disk.qcow2", "disk.raw", "vars.fd", "efi-vars")
	}
	for _, f := range rm {
		_ = os.Remove(filepath.Join(dir, f))
	}
}

func listenEndpoint(ep backend.Endpoint) (net.Listener, error) {
	if ep.Network == "unix" {
		_ = os.Remove(ep.Addr)
		l, err := net.Listen("unix", ep.Addr)
		if err == nil {
			_ = os.Chmod(ep.Addr, 0o600)
		}
		return l, err
	}
	return net.Listen(ep.Network, ep.Addr)
}

func addrOf(ep backend.Endpoint, l net.Listener) string {
	if ep.Network == "unix" {
		return ep.Addr
	}
	return l.Addr().String()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// cachedKey resolves the real API key on first use and refreshes it every few
// minutes, so slow keychain lookups do not run per request.
func cachedKey(ref string) func() (string, error) {
	var mu sync.Mutex
	var val string
	var at time.Time
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if val != "" && time.Since(at) < 5*time.Minute {
			return val, nil
		}
		if ref == "" {
			return "", errors.New("credentials.llm_gateway.key_ref is not set in the policy")
		}
		v, err := secrets.Resolve(ref)
		if err != nil {
			return "", err
		}
		val, at = v, time.Now()
		return val, nil
	}
}

func serveAudit(l net.Listener, lg *audit.Logger) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			var req struct {
				Type   string         `json:"type"`
				Fields map[string]any `json:"fields"`
			}
			if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil || req.Type == "" {
				fmt.Fprintln(c, "bad request")
				return
			}
			if err := lg.Write(req.Type, req.Fields); err != nil {
				fmt.Fprintln(c, "write failed")
				return
			}
			fmt.Fprintln(c, "ok")
		}(c)
	}
}

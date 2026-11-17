package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"glasshouse/internal/backend"
	"glasshouse/internal/cloudinit"
	"glasshouse/internal/fsbridge"
	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

func splitDashDash(args []string) (before, after []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func cmdRun(args []string) (err error) {
	args, command := splitDashDash(args)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	policyFile := fs.String("policy", "", "user policy file")
	modeFlag := fs.String("mode", "", "launch mode: gui (desktop) or shell (lightweight SSH shell)")
	guiFlag := fs.Bool("gui", false, "desktop mode: GUI VM, opens the desktop window")
	shellFlag := fs.Bool("shell", false, "shell mode: lightweight VM without a desktop, drops into a shell")
	headless := fs.Bool("headless", false, "alias of --shell")
	offline := fs.Bool("offline", false, "no outbound network")
	keep := fs.Bool("keep", false, "keep session disk after stop")
	tcg := fs.Bool("allow-tcg", false, "allow software emulation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("at most one project directory may be given")
	}
	project := ""
	if fs.NArg() == 1 {
		if project, err = filepath.Abs(fs.Arg(0)); err != nil {
			return err
		}
		if st, serr := os.Stat(project); serr != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a directory", fs.Arg(0))
		}
	}

	mode, err := resolveMode(*modeFlag, *guiFlag, *shellFlag, *headless)
	if err != nil {
		return err
	}
	pol, notes, err := effectivePolicy(*policyFile, project, mode == modeShell, *offline)
	if err != nil {
		return err
	}
	if err := checkModePolicy(mode, pol); err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "policy:", n)
	}
	for _, w := range pol.Warnings() {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	home, err := sessionHome()
	if err != nil {
		return err
	}
	be, err := hostBackend()
	if err != nil {
		return err
	}
	arch, err := hostArch()
	if err != nil {
		return err
	}
	id, err := session.NewID()
	if err != nil {
		return err
	}
	if len(session.Dir(home, id))+len("/gateway.sock") > 100 {
		return fmt.Errorf("GLASSHOUSE_HOME path is too long for unix sockets; set GLASSHOUSE_HOME to a shorter directory")
	}
	dir, err := session.Create(home, id)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = session.Remove(home, id)
		}
	}()

	token, err := session.NewToken()
	if err != nil {
		return err
	}
	gui := pol.VM.Profile == "desktop"
	spec := backend.Spec{
		ID: id, Dir: dir, Arch: arch, CPUs: pol.VM.CPUs, MemoryMiB: int(pol.VM.Memory >> 20),
		DiskBytes: pol.VM.Disk, GUI: gui, Offline: pol.Network.Mode == "offline", HostNet: pol.Network.Mode == "host", AllowTCG: *tcg,
		Disk: filepath.Join(dir, "disk."+be.ImageExt()), Seed: filepath.Join(dir, "seed.iso"),
	}
	if err := be.Preflight(&spec); err != nil {
		return err
	}
	spec.Base = session.ImagePath(home, arch, pol.VM.Profile, be.ImageExt())
	fmt.Fprintln(os.Stderr, "verifying base image...")
	if err := session.VerifyImage(spec.Base); err != nil {
		return err
	}
	baseSHA, err := session.ImageSHA(spec.Base)
	if err != nil {
		return err
	}
	if err := be.PrepareDisk(&spec); err != nil {
		return err
	}
	if err := writeSeed(be, spec, pol, token, 0); err != nil {
		return err
	}
	if err := writePolicySnapshot(filepath.Join(dir, "policy.json"), pol); err != nil {
		return err
	}
	info := session.Info{
		ID: id, Created: time.Now().UTC(), Backend: be.Name(), Arch: arch, Profile: pol.VM.Profile,
		Project: project, Persistent: *keep, BaseSHA256: baseSHA, GUI: gui, Offline: spec.Offline, HostNet: spec.HostNet, CCMonitor: pol.Monitor.CCMonitor,
		CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB, DiskBytes: spec.DiskBytes, Token: token,
		State: "creating", RootSSH: pol.SSH.Root, AllowTCG: *tcg,
	}
	if err := session.Save(home, info); err != nil {
		return err
	}

	return launchSession(home, be, info, pol, mode, command, false, func() { ok = true }) // from then on the supervisor owns cleanup
}

func importProject(home string, in session.Info, deny []string, maxBytes int64) error {
	dir := session.Dir(home, in.ID)
	argv, err := sshArgv(home, in, "root")
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var rep fsbridge.Report
	packErr := make(chan error, 1)
	go func() {
		var err error
		rep, err = fsbridge.Pack(in.Project, pw, fsbridge.Options{DenyPatterns: deny, MaxBytes: maxBytes, MaxFiles: 200000})
		pw.CloseWithError(err)
		packErr <- err
	}()
	cmd := exec.Command(argv[0], append(argv[1:], "tar -xf - -C /workspace --no-same-owner && chown -R agent:agent /workspace")...)
	cmd.Stdin = pr
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	pr.Close()
	if perr := <-packErr; perr != nil {
		return perr
	}
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(os.Stderr, "imported %d files (%s) into /workspace", rep.Files, humanBytes(rep.Bytes))
	if len(rep.Skipped) > 0 {
		fmt.Fprintf(os.Stderr, ", skipped %d", len(rep.Skipped))
	}
	fmt.Fprintln(os.Stderr)
	skipped := rep.Skipped
	if len(skipped) > 100 {
		skipped = skipped[:100]
	}
	return auditEvent(dir, "files.import", map[string]any{
		"project": in.Project, "files": rep.Files, "bytes": rep.Bytes, "skipped": skipped,
	})
}

// runInGuest runs a command as the unprivileged agent user in /workspace with
// the session environment (gateway URL, placeholder key, proxy).
func runInGuest(home string, in session.Info, command []string) error {
	argv, err := sshArgv(home, in, "agent")
	if err != nil {
		return err
	}
	quoted := make([]string, len(command))
	for i, c := range command {
		quoted[i] = shellQuote(c)
	}
	remote := "bash -lc " + shellQuote(`set -a; . /etc/environment; set +a; cd /workspace && exec "$@"`) + " _ " + strings.Join(quoted, " ")
	if err := auditEvent(session.Dir(home, in.ID), "exec", map[string]any{"user": "agent", "argv": command}); err != nil {
		return err
	}
	path, err := lookOrErr(argv[0])
	if err != nil {
		return err
	}
	argv = append(argv[:1], append([]string{"-t"}, argv[1:]...)...)
	return syscall.Exec(path, append(argv, remote), os.Environ())
}

// launchSession starts the supervisor of a prepared session, waits until the guest is ready and then hands over to the
// requested mode. resumed is true for a kept session started again: its /workspace is already on the disk.
// started is called once the supervisor owns the session's cleanup.
func launchSession(home string, be backend.Backend, info session.Info, pol policy.Policy, mode string, command []string, resumed bool, started func()) (err error) {
	id, gui := info.ID, info.GUI
	dir := session.Dir(home, id)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	sup := exec.Command(exe, "_supervise", id)
	sup.Stdout, sup.Stderr = logf, logf
	sup.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sup.Start(); err != nil {
		return err
	}
	logf.Close()
	started()

	if resumed {
		fmt.Fprintf(os.Stderr, "session %s resuming...\n", id)
	} else {
		fmt.Fprintf(os.Stderr, "session %s starting (first boot can take a minute or two)...\n", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if info, err = waitReady(ctx, home, id); err != nil {
		if resumed {
			return fmt.Errorf("%w\n(session %s and its disk are kept: glasshouse logs %s; try glasshouse start %s again, or remove it with glasshouse rm %s)", err, id, id, id, id)
		}
		return fmt.Errorf("%w\n(session %s kept for inspection: glasshouse logs %s; remove with glasshouse rm %s)", err, id, id, id)
	}

	if !resumed && info.Project != "" && pol.Files.Mode == "copy" { // a resumed disk already holds /workspace
		if err := importProject(home, info, pol.Files.DenyPatterns, pol.Files.MaxImport); err != nil {
			return fmt.Errorf("import failed: %w (session %s is running)", err, id)
		}
	}
	fmt.Fprintf(os.Stderr, "session %s ready\n", id)

	if len(command) == 0 && !gui && mode == modeShell {
		// Lightweight shell mode: no command given, so hand the terminal to an agent shell in /workspace.
		fmt.Fprintf(os.Stderr, "shell mode: entering an agent shell in /workspace (session %s keeps running after you exit;\n  glasshouse pull %s to bring changes back, glasshouse stop %s to shut it down)\n", id, id, id)
		command = []string{"bash", "-l"}
	}
	if len(command) > 0 {
		return runInGuest(home, info, command)
	}
	if gui {
		if err := be.OpenDisplay(specFromInfo(home, info, be)); err != nil {
			fmt.Fprintln(os.Stderr, "note:", err)
		}
	}
	fmt.Printf("session %s is running\n  glasshouse ssh %s      root shell\n  glasshouse gui %s      desktop\n  glasshouse pull %s     bring changes back\n  glasshouse stop %s\n", id, id, id, id, id)
	return nil
}

// writeSeed generates the session's SSH key and writes the cloud-init seed. boot is 0 on the first boot and counts
// resumes afterwards, so cloud-init treats each resume as a new instance and installs the new key and token.
func writeSeed(be backend.Backend, spec backend.Spec, pol policy.Policy, token string, boot int) error {
	pub, err := session.GenKey(spec.Dir, spec.ID)
	if err != nil {
		return err
	}
	gn := be.GuestNet(spec)
	if pol.Credentials.LLMGateway.KeyRef == "" {
		// No API key configured: do not point Claude Code at the gateway or hand it a placeholder key. Either would
		// override its own login, and subscription (claude.ai) login is what is wanted in that case.
		gn.GatewayURL = ""
	}
	ud, md, err := cloudinit.Render(cloudinit.Params{
		ID: spec.ID, Boot: boot, PublicKey: pub, Token: token, Profile: pol.VM.Profile, RootSSH: pol.SSH.Root,
		ProxyHost: gn.ProxyHost, ProxyPort: gn.ProxyPort, GatewayURL: gn.GatewayURL,
		VsockBridge: gn.VsockBridge, CCMonitor: pol.Monitor.CCMonitor,
	})
	if err != nil {
		return err
	}
	return cloudinit.BuildISO(spec.Seed, ud, md)
}

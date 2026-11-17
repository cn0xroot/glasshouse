package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"glasshouse/internal/audit"
	"glasshouse/internal/fsbridge"
	"glasshouse/internal/session"
)

func cmdSSH(args []string) error {
	args, command := splitDashDash(args)
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	user := fs.String("user", "root", "guest user: root or agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user != "root" && *user != "agent" {
		return errors.New("--user must be root or agent")
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	in, err := loadRunning(home, fs.Arg(0))
	if err != nil {
		return err
	}
	if *user == "root" && !in.RootSSH {
		return errors.New("policy ssh.root is false for this session; use --user agent")
	}
	argv, err := sshArgv(home, in, *user)
	if err != nil {
		return err
	}
	if err := auditEvent(session.Dir(home, in.ID), "ssh.login", map[string]any{"user": *user, "argv": command}); err != nil {
		return err
	}
	path, err := lookOrErr(argv[0])
	if err != nil {
		return err
	}
	if len(command) > 0 {
		quoted := make([]string, len(command))
		for i, c := range command {
			quoted[i] = shellQuote(c)
		}
		argv = append(argv, strings.Join(quoted, " "))
	}
	return syscall.Exec(path, argv, os.Environ())
}

func cmdGUI(args []string) error {
	home, err := sessionHome()
	if err != nil {
		return err
	}
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	in, err := loadRunning(home, id)
	if err != nil {
		return err
	}
	be, err := backendByName(in.Backend)
	if err != nil {
		return err
	}
	return be.OpenDisplay(specFromInfo(home, in, be))
}

func cmdPull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "apply without asking (only when files.export is review)")
	del := fs.Bool("delete", false, "also delete project files that no longer exist in the guest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	in, err := loadRunning(home, fs.Arg(0))
	if err != nil {
		return err
	}
	project := in.Project
	if fs.NArg() > 1 {
		if project, err = filepath.Abs(fs.Arg(1)); err != nil {
			return err
		}
	}
	if project == "" {
		return errors.New("this session has no project directory; pass one: glasshouse pull <session> <dir>")
	}
	pol, err := loadSessionPolicy(home, in.ID)
	if err != nil {
		return err
	}
	dir := session.Dir(home, in.ID)
	if pol.Files.Export == "deny" {
		_ = auditEvent(dir, "files.export.denied", map[string]any{"reason": "policy"})
		return errors.New("policy files.export=deny: nothing may leave the guest")
	}

	staging, err := os.MkdirTemp(dir, "staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	argv, err := sshArgv(home, in, "root")
	if err != nil {
		return err
	}
	if !in.RootSSH {
		if argv, err = sshArgv(home, in, "agent"); err != nil {
			return err
		}
	}
	cmd := exec.Command(argv[0], append(argv[1:], "tar -C /workspace -cf - .")...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// The guest's tar output is untrusted: parse it ourselves.
	rep, xerr := fsbridge.Extract(out, staging, fsbridge.Options{
		DenyPatterns: pol.Files.DenyPatterns, MaxBytes: pol.Files.MaxImport, MaxFiles: 200000,
	})
	if xerr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("rejected guest archive: %w", xerr)
	}
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("could not read /workspace from the guest: %w", err)
	}

	changes, err := fsbridge.Diff(project, staging, pol.Files.DenyPatterns, *del)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Println("no changes")
		return nil
	}
	fmt.Printf("%d change(s) from the guest (%d files received, %d skipped):\n", len(changes), rep.Files, len(rep.Skipped))
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		note := ""
		switch {
		case c.ModeOnly:
			note = "  (permissions only)"
		case c.ExecBit:
			note = "  (executable bit)"
		}
		fmt.Printf("  %-9s %s%s\n", c.Kind, c.Path, note)
		paths = append(paths, c.Path)
	}
	if len(paths) > 200 {
		paths = paths[:200]
	}
	if pol.Files.Export == "review" && !*yes {
		if !confirm("Apply these changes to " + project + "?") {
			_ = auditEvent(dir, "files.export.declined", map[string]any{"changes": len(changes)})
			fmt.Println("nothing written")
			return nil
		}
	}
	// Record first: if the audit log cannot take the event, nothing is written.
	if err := auditEvent(dir, "files.export", map[string]any{
		"project": project, "changes": len(changes), "paths": paths, "mode": pol.Files.Export,
	}); err != nil {
		return err
	}
	if err := fsbridge.Apply(staging, project, changes); err != nil {
		return err
	}
	fmt.Println("applied")
	return nil
}

func cmdLS(args []string) error {
	home, err := sessionHome()
	if err != nil {
		return err
	}
	all, err := session.List(home)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tBACKEND\tPROFILE\tAGE\tEGRESS\tLLM\tPROJECT")
	for _, s := range all {
		state := s.State
		if (state == "ready" || state == "starting") && !s.Alive() {
			state = "dead"
		}
		eg, llm := "-", "-"
		if st, err := session.LoadStats(home, s.ID); err == nil {
			eg, llm = humanBytes(st.Egress), fmt.Sprint(st.LLMRequests)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, state, s.Backend, s.Profile, humanAge(s.Created), eg, llm, s.Project)
	}
	return tw.Flush()
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "keep printing new events")
	verify := fs.Bool("verify", false, "verify the hash chain and exit")
	console := fs.Bool("console", false, "print the guest serial console instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	in, err := session.Resolve(home, fs.Arg(0))
	if err != nil {
		return err
	}
	dir := session.Dir(home, in.ID)
	if *console {
		f, err := os.Open(filepath.Join(dir, "console.log"))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	}
	path := filepath.Join(dir, "audit.jsonl")
	if *verify {
		n, err := audit.Verify(path)
		if err != nil {
			return fmt.Errorf("AUDIT LOG FAILED VERIFICATION after %d records: %w", n, err)
		}
		fmt.Printf("audit log OK: %d records, chain intact\n", n)
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 && strings.HasSuffix(line, "\n") {
			printRecord(line)
		}
		if err == io.EOF {
			if !*follow {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func printRecord(line string) {
	var rec audit.Record
	if json.Unmarshal([]byte(line), &rec) != nil {
		fmt.Print(line)
		return
	}
	keys := make([]string, 0, len(rec.Fields))
	for k := range rec.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, rec.Fields[k])
	}
	ts := rec.Time
	if t, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil {
		ts = t.Local().Format("15:04:05")
	}
	fmt.Printf("%s %-20s%s\n", ts, rec.Type, b.String())
}

func cmdStop(args []string) error {
	home, err := sessionHome()
	if err != nil {
		return err
	}
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	in, err := session.Resolve(home, id)
	if err != nil {
		return err
	}
	if !in.Alive() {
		fmt.Printf("session %s is not running (state: %s)\n", in.ID, in.State)
		return nil
	}
	if err := syscall.Kill(in.Supervisor, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := session.Load(home, in.ID)
		if err == nil && !cur.Alive() {
			fmt.Printf("session %s stopped\n", in.ID)
			if in.Persistent {
				fmt.Printf("its disk is kept: glasshouse start %s boots it again\n", in.ID)
			}
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("session %s did not stop within 90s", in.ID)
}

func cmdRM(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: glasshouse rm <session>")
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	in, err := session.Load(home, args[0])
	if err != nil {
		return err
	}
	if in.Alive() {
		return fmt.Errorf("session %s is running; stop it first", in.ID)
	}
	if err := session.Remove(home, in.ID); err != nil {
		return err
	}
	fmt.Println("removed", in.ID)
	return nil
}

func cmdPolicy(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: glasshouse policy check [file]")
	}
	file := ""
	if len(args) > 1 {
		file = args[1]
	}
	p, notes, err := effectivePolicy(file, "", false, false)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("note:", n)
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	ws := p.Warnings()
	for _, w := range ws {
		fmt.Println("warning:", w)
	}
	if len(ws) == 0 {
		fmt.Println("no isolation warnings")
	}
	return nil
}

func cmdDoctor(args []string) error {
	bad := 0
	check := func(name string, err error) {
		if err != nil {
			bad++
			fmt.Printf("  FAIL  %-28s %v\n", name, err)
			return
		}
		fmt.Printf("  ok    %s\n", name)
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	arch, err := hostArch()
	if err != nil {
		return err
	}
	be, err := hostBackend()
	if err != nil {
		return err
	}
	fmt.Printf("host: %s/%s, backend: %s, data: %s\n", runtime.GOOS, arch, be.Name(), home)
	for _, t := range []string{"ssh", "ssh-keygen"} {
		_, err := lookOrErr(t)
		check(t, err)
	}
	var isoErr error = errors.New("none of xorriso, genisoimage, mkisofs, hdiutil found")
	for _, t := range []string{"xorriso", "genisoimage", "mkisofs", "hdiutil"} {
		if _, err := exec.LookPath(t); err == nil {
			isoErr = nil
			break
		}
	}
	check("ISO tool (cloud-init seed)", isoErr)
	if runtime.GOOS == "darwin" {
		_, err := lookOrErr("nc")
		check("nc (ssh over unix socket)", err)
	}
	pol, _, perr := effectivePolicy("", "", false, false)
	check("policy loads", perr)
	spec := backendSpecForDoctor(arch, pol)
	check("hypervisor ("+be.Name()+")", be.Preflight(&spec))
	for _, profile := range []string{"headless", "desktop"} {
		img := session.ImagePath(home, arch, profile, be.ImageExt())
		check("image "+filepath.Base(img), session.VerifyImage(img))
	}
	if perr == nil && pol.Credentials.LLMGateway.KeyRef == "" {
		fmt.Println("  note  credentials.llm_gateway.key_ref is not set: the model gateway will refuse requests")
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	fmt.Println("all checks passed")
	return nil
}

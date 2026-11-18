package main

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
	"strings"
	"time"

	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

func writePolicySnapshot(path string, p policy.Policy) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readPolicySnapshot(path string) (policy.Policy, error) {
	var p policy.Policy
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func confirm(prompt string) bool {
	fmt.Print(prompt + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	l := strings.ToLower(strings.TrimSpace(line))
	return l == "y" || l == "yes"
}

// auditEvent sends one record to the session supervisor, which owns the log.
// It is an error if the event cannot be recorded: callers must not proceed.
func auditEvent(dir, typ string, fields map[string]any) error {
	c, err := net.DialTimeout("unix", filepath.Join(dir, "audit.sock"), 3*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach the session audit log: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(map[string]any{"type": typ, "fields": fields}); err != nil {
		return err
	}
	resp, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp) != "ok" {
		return fmt.Errorf("audit log refused the event: %s", strings.TrimSpace(resp))
	}
	return nil
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1fGiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1fMiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1fKiB", float64(n)/k)
	}
	return fmt.Sprintf("%dB", n)
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func loadRunning(home, id string) (session.Info, error) {
	in, err := session.Resolve(home, id)
	if err != nil {
		return in, err
	}
	if !in.Alive() || in.State != "ready" {
		return in, fmt.Errorf("session %s is not running (state: %s)", in.ID, in.State)
	}
	return in, nil
}

func waitReady(ctx context.Context, home, id string) (session.Info, error) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		in, err := session.Load(home, id)
		if err == nil {
			switch {
			case in.State == "ready":
				return in, nil
			case in.State == "failed":
				return in, fmt.Errorf("session failed: %s", in.Error)
			case in.Supervisor > 0 && !in.Alive():
				return in, fmt.Errorf("supervisor exited unexpectedly; see %s", filepath.Join(session.Dir(home, id), "supervisor.log"))
			}
		}
		select {
		case <-ctx.Done():
			return in, fmt.Errorf("timed out waiting for the VM: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// guestConfiguredScript runs in the guest and succeeds once the first-boot setup is done: cloud-init has finished its
// last stage (users, keys, /workspace, services) and the two things the host relies on right away exist. A session is
// "ready" as soon as sshd answers, which is earlier: /workspace is created by cloud-init's runcmd, in its final stage, so
// a project import started at "ready" could find no /workspace (tar exits 2).
const guestConfiguredScript = `cloud-init status --wait >/dev/null 2>&1; for i in $(seq 1 60); do [ -d /workspace ] && id agent >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1`

// waitGuestConfigured waits, over ssh, until the guest has finished its first-boot setup. ssh exiting with 255 means
// ssh itself failed (sshd is up but the session key is not installed yet): that is retried until limit. Any other
// failure is the script's verdict and is returned at once.
func waitGuestConfigured(home string, in session.Info, limit time.Duration) error {
	user := "root"
	if !in.RootSSH {
		user = "agent"
	}
	deadline := time.Now().Add(limit)
	for {
		argv, err := sshArgv(home, in, user)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		out, err := exec.CommandContext(ctx, argv[0], append(argv[1:], guestConfiguredScript)...).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 255 || time.Now().After(deadline) {
			return fmt.Errorf("the guest did not finish its first-boot setup: %v: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(time.Second)
	}
}

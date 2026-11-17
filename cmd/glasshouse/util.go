package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
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

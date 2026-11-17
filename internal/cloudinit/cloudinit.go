// Package cloudinit renders the per-session NoCloud seed: an ephemeral SSH key,
// proxy/gateway environment, sshd hardening and the optional CC-Monitor guard.
package cloudinit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Params struct {
	ID          string
	Boot        int    // >0 when resuming a kept session: a new instance-id makes cloud-init apply the new key and token
	PublicKey   string // single-line OpenSSH public key
	Token       string // per-session secret: proxy password and the guest's placeholder API key
	Profile     string // headless | desktop
	RootSSH     bool
	ProxyHost   string // "" when offline
	ProxyPort   int
	GatewayURL  string
	VsockBridge bool // macOS: guest bridges loopback ports and sshd to vsock
	CCMonitor   bool
}

var (
	idRe  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	keyRe = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/=]+( [^\n\r]*)?$`)
	tokRe = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
)

func (p Params) validate() error {
	if !idRe.MatchString(p.ID) {
		return errors.New("bad session id")
	}
	if p.Boot < 0 {
		return errors.New("bad boot counter")
	}
	if !keyRe.MatchString(strings.TrimSpace(p.PublicKey)) {
		return errors.New("public key must be a single-line ssh-ed25519 key")
	}
	if !tokRe.MatchString(p.Token) {
		return errors.New("token must be 16-128 alphanumeric characters")
	}
	if p.Profile != "headless" && p.Profile != "desktop" {
		return errors.New("bad profile")
	}
	// An empty gateway URL means "no API key configured": subscription login mode, nothing is injected.
	if strings.ContainsAny(p.GatewayURL, " \n\r\"'") {
		return errors.New("bad gateway url")
	}
	return nil
}

func Render(p Params) (userData, metaData string, err error) {
	if err := p.validate(); err != nil {
		return "", "", err
	}
	key := strings.TrimSpace(p.PublicKey)
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("#cloud-config")
	w("hostname: glasshouse-%s", p.ID)
	w("ssh_pwauth: false")
	w("disable_root: false")
	w("users:")
	w("  - name: root")
	w("    lock_passwd: true")
	w("    ssh_authorized_keys: [\"%s\"]", key)
	w("  - name: agent")
	w("    shell: /bin/bash")
	w("    groups: [sudo]")
	w("    sudo: ALL=(ALL) NOPASSWD:ALL")
	w("    lock_passwd: true")
	w("    ssh_authorized_keys: [\"%s\"]", key)
	w("write_files:")

	// Environment seen by login shells and ssh sessions.
	// The guest only reaches the model gateway (it forwards /v1/ only), so Claude Code's self-update and
	// non-essential traffic (telemetry, update checks) can only fail noisily: turn them off.
	env := []string{"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if p.GatewayURL != "" {
		// API-key mode: Claude Code talks to the host gateway with a placeholder key. Without a gateway key these
		// are left out so that Claude Code's own subscription login (claude.ai) is used.
		env = append(env, "ANTHROPIC_BASE_URL="+p.GatewayURL, "ANTHROPIC_API_KEY="+p.Token)
	}
	if p.ProxyHost != "" {
		proxy := fmt.Sprintf("http://glasshouse:%s@%s:%d", p.Token, p.ProxyHost, p.ProxyPort)
		env = append(env,
			"http_proxy="+proxy, "https_proxy="+proxy, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy,
			"no_proxy=localhost,127.0.0.1", "NO_PROXY=localhost,127.0.0.1")
	}
	w("  - path: /etc/environment")
	w("    permissions: '0644'")
	w("    content: |")
	w("      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	for _, e := range env {
		w("      %s", e)
	}

	if p.ProxyHost != "" {
		proxy := fmt.Sprintf("http://glasshouse:%s@%s:%d", p.Token, p.ProxyHost, p.ProxyPort)
		w("  - path: /etc/apt/apt.conf.d/90glasshouse-proxy")
		w("    permissions: '0644'")
		w("    content: |")
		w("      Acquire::http::Proxy \"%s\";", proxy)
		w("      Acquire::https::Proxy \"%s\";", proxy)
	}

	root := "prohibit-password"
	if !p.RootSSH {
		root = "no"
	}
	w("  - path: /etc/ssh/sshd_config.d/10-glasshouse.conf")
	w("    permissions: '0644'")
	w("    content: |")
	w("      PermitRootLogin %s", root)
	w("      PasswordAuthentication no")
	w("      KbdInteractiveAuthentication no")
	w("      X11Forwarding no")
	w("      AllowAgentForwarding no")
	w("      AllowTcpForwarding local")

	if p.Profile == "desktop" {
		w("  - path: /etc/gdm3/custom.conf")
		w("    permissions: '0644'")
		w("    content: |")
		w("      [daemon]")
		w("      AutomaticLoginEnable=true")
		w("      AutomaticLogin=agent")
		w("      WaylandEnable=false")
		// The agent account has no password, so a locked screen could never be unlocked again, and an idle suspend would
		// freeze the VM: no screen lock, no blanking, no suspend. dconf system defaults, applied by `dconf update` below.
		w("  - path: /etc/dconf/profile/user")
		w("    permissions: '0644'")
		w("    content: |")
		w("      user-db:user")
		w("      system-db:local")
		w("  - path: /etc/dconf/db/local.d/00-glasshouse")
		w("    permissions: '0644'")
		w("    content: |")
		w("      [org/gnome/desktop/screensaver]")
		w("      lock-enabled=false")
		w("      idle-activation-enabled=false")
		w("      [org/gnome/desktop/session]")
		w("      idle-delay=uint32 0")
		w("      [org/gnome/settings-daemon/plugins/power]")
		w("      sleep-inactive-ac-type='nothing'")
		w("      sleep-inactive-battery-type='nothing'")
		w("      idle-dim=false")
	}

	if p.VsockBridge {
		unit := func(name, desc, exec string) {
			w("  - path: /etc/systemd/system/%s.service", name)
			w("    permissions: '0644'")
			w("    content: |")
			w("      [Unit]")
			w("      Description=%s", desc)
			w("      After=network.target")
			w("      [Service]")
			w("      ExecStart=%s", exec)
			w("      Restart=always")
			w("      [Install]")
			w("      WantedBy=multi-user.target")
		}
		unit("glasshouse-vsock-ssh", "Glasshouse: sshd over vsock", "/usr/bin/socat VSOCK-LISTEN:22,fork,reuseaddr TCP:127.0.0.1:22")
		unit("glasshouse-vsock-gateway", "Glasshouse: model gateway over vsock", "/usr/bin/socat TCP-LISTEN:8080,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:8080")
		if p.ProxyHost != "" {
			unit("glasshouse-vsock-proxy", "Glasshouse: egress proxy over vsock", "/usr/bin/socat TCP-LISTEN:3128,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:3128")
		}
	}

	w("runcmd:")
	w("  - [ mkdir, -p, /workspace ]")
	w("  - [ chown, 'agent:agent', /workspace ]")
	w("  - [ systemctl, restart, ssh ]")
	if p.Profile == "desktop" {
		w("  - [ dconf, update ]")
	}
	if p.VsockBridge {
		w("  - [ systemctl, daemon-reload ]")
		w("  - [ systemctl, enable, --now, glasshouse-vsock-ssh, glasshouse-vsock-gateway ]")
		if p.ProxyHost != "" {
			w("  - [ systemctl, enable, --now, glasshouse-vsock-proxy ]")
		}
	}
	if p.CCMonitor {
		// The image build installs Claude Code first and then CC-Monitor, and hooks Claude Code at that point: the hooks
		// live in /etc/claude-code/managed-settings.json (root-owned, applies to every user). So there is nothing to
		// register here, only the web UI service to start. The home directory fix is for images built by older scripts
		// that left /home/agent root-owned.
		w("  - [ chown, agent:agent, /home/agent ]")
		// Fallback for an image built before the hooks were baked in: without this such a session would run with no
		// monitoring at all and nothing would say so. It does nothing when the image already has the hooks.
		w("  - [ sh, -c, \"test -s /etc/claude-code/managed-settings.json || { mkdir -p /etc/claude-code && python3 /opt/cc-monitor/install.py --target /etc/claude-code/managed-settings.json --skip-statusline; }\" ]")
		// The unit is ordered After=cloud-final, and this runs inside cloud-final: --now without --no-block deadlocks.
		w("  - [ systemctl, enable, --now, --no-block, glasshouse-cc-monitor ]")
	} else {
		// monitor.cc_monitor is off for this session: take the hooks baked into the image out again.
		w("  - [ rm, -f, /etc/claude-code/managed-settings.json ]")
	}

	instance := "glasshouse-" + p.ID
	if p.Boot > 0 {
		instance = fmt.Sprintf("%s-%d", instance, p.Boot)
	}
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: glasshouse-%s\n", instance, p.ID)
	return b.String(), meta, nil
}

// BuildISO writes a NoCloud seed (volume label "cidata") to out using whichever
// ISO tool the host has.
func BuildISO(out, userData, metaData string) error {
	dir, err := os.MkdirTemp("", "glasshouse-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userData), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return err
	}
	ud, md := filepath.Join(dir, "user-data"), filepath.Join(dir, "meta-data")
	candidates := [][]string{
		{"xorriso", "-as", "mkisofs", "-o", out, "-V", "cidata", "-J", "-r", ud, md},
		{"genisoimage", "-quiet", "-o", out, "-V", "cidata", "-J", "-r", ud, md},
		{"mkisofs", "-quiet", "-o", out, "-V", "cidata", "-J", "-r", ud, md},
		{"hdiutil", "makehybrid", "-quiet", "-iso", "-joliet", "-default-volume-name", "cidata", "-o", out, dir},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		if outb, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s failed: %v: %s", c[0], err, strings.TrimSpace(string(outb)))
		}
		// hdiutil appends .iso when missing
		if _, err := os.Stat(out); err != nil {
			if _, err2 := os.Stat(out + ".iso"); err2 == nil {
				return os.Rename(out+".iso", out)
			}
		}
		return os.Chmod(out, 0o600)
	}
	return errors.New("no ISO tool found: install xorriso (or genisoimage); macOS has hdiutil built in")
}

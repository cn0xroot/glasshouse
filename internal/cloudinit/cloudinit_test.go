package cloudinit

import (
	"strings"
	"testing"
)

func base() Params {
	return Params{
		ID: "s-abc123", PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample glasshouse-s-abc123",
		Token: "0123456789abcdef0123456789abcdef", Profile: "desktop", RootSSH: true,
		ProxyHost: "10.0.2.100", ProxyPort: 3128, GatewayURL: "http://10.0.2.101:8080", CCMonitor: true,
	}
}

func TestRenderContents(t *testing.T) {
	ud, md, err := Render(base())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config", "ssh_pwauth: false", "disable_root: false",
		"PermitRootLogin prohibit-password", "PasswordAuthentication no", "AllowAgentForwarding no",
		"ANTHROPIC_BASE_URL=http://10.0.2.101:8080",
		"ANTHROPIC_API_KEY=0123456789abcdef0123456789abcdef",
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"https_proxy=http://glasshouse:0123456789abcdef0123456789abcdef@10.0.2.100:3128",
		"Acquire::http::Proxy",
		"AutomaticLogin=agent",
		"glasshouse-cc-monitor",
		// regressions found on the first real boot: root-owned /home/agent, and a cloud-final deadlock
		"[ chown, agent:agent, /home/agent ]", "enable, --now, --no-block, glasshouse-cc-monitor",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(ud, "vsock") {
		t.Error("qemu backend must not install vsock bridges")
	}
	if !strings.Contains(md, "instance-id: glasshouse-s-abc123") {
		t.Errorf("meta-data: %s", md)
	}
}

func TestRenderVariants(t *testing.T) {
	p := base()
	p.RootSSH, p.Profile, p.CCMonitor, p.ProxyHost = false, "headless", false, ""
	ud, _, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, "PermitRootLogin no") {
		t.Error("root ssh should be disabled")
	}
	// monitor off: the hooks baked into the image are removed, and nothing mentions the monitor service
	if !strings.Contains(ud, "rm, -f, /etc/claude-code/managed-settings.json") {
		t.Errorf("a session without the monitor must remove the baked-in hooks:\n%s", ud)
	}
	if strings.Contains(ud, "gdm3") || strings.Contains(ud, "cc-monitor") || strings.Contains(ud, "proxy") {
		t.Errorf("headless/offline/no-monitor leaked config:\n%s", ud)
	}
	p = base()
	p.VsockBridge = true
	ud, _, _ = Render(p)
	for _, want := range []string{"VSOCK-LISTEN:22", "VSOCK-CONNECT:2:8080", "VSOCK-CONNECT:2:3128"} {
		if !strings.Contains(ud, want) {
			t.Errorf("vsock bridge missing %q", want)
		}
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	cases := map[string]func(*Params){
		"id newline":   func(p *Params) { p.ID = "x\nruncmd: evil" },
		"key newline":  func(p *Params) { p.PublicKey = "ssh-ed25519 AAAA\nruncmd: [evil]" },
		"key quote":    func(p *Params) { p.PublicKey = "ssh-rsa AAAA" },
		"token short":  func(p *Params) { p.Token = "abc" },
		"token symbol": func(p *Params) { p.Token = "0123456789abcdef0123456789abcde\"" },
		"gateway":      func(p *Params) { p.GatewayURL = "http://x\"\n" },
		"profile":      func(p *Params) { p.Profile = "root" },
	}
	for name, mut := range cases {
		p := base()
		mut(&p)
		if _, _, err := Render(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRenderSubscriptionLoginMode(t *testing.T) {
	// No gateway key configured: the guest must not be pointed at the gateway nor given a placeholder key,
	// otherwise Claude Code would use that instead of its own subscription login.
	p := base()
	p.GatewayURL = ""
	ud, _, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ud, "ANTHROPIC_API_KEY") || strings.Contains(ud, "ANTHROPIC_BASE_URL") {
		t.Errorf("ANTHROPIC_* must not be injected without a gateway key:\n%s", ud)
	}
	if !strings.Contains(ud, "DISABLE_AUTOUPDATER=1") {
		t.Error("auto-update must still be disabled")
	}
	// with a gateway the placeholder key is still injected
	ud, _, _ = Render(base())
	if !strings.Contains(ud, "ANTHROPIC_API_KEY=0123456789abcdef0123456789abcdef") {
		t.Error("API-key mode must keep the placeholder key")
	}
}

func TestRenderMonitorHooksAreBakedIntoTheImage(t *testing.T) {
	// With the monitor on, the hooks come from the image (managed-settings.json, written at image build time):
	// the session must neither re-register them nor delete them.
	ud, _, err := Render(base())
	if err != nil {
		t.Fatal(err)
	}
	// the only mention of install.py is the guarded fallback for images that lack the hooks
	if strings.Count(ud, "install.py") != 1 || !strings.Contains(ud, "test -s /etc/claude-code/managed-settings.json || {") {
		t.Errorf("hooks must come from the image; install.py may only run as a guarded fallback:\n%s", ud)
	}
	if strings.Contains(ud, "rm, -f, /etc/claude-code/managed-settings.json") {
		t.Errorf("the baked-in hooks must stay when the monitor is on:\n%s", ud)
	}
}

func TestRenderBootCounterChangesInstanceID(t *testing.T) {
	// A resumed session must look like a new instance to cloud-init, or it would keep the old key and token.
	_, md, _ := Render(base())
	if !strings.Contains(md, "instance-id: glasshouse-s-abc123\n") {
		t.Errorf("first boot: %s", md)
	}
	p := base()
	p.Boot = 2
	_, md, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "instance-id: glasshouse-s-abc123-2\n") {
		t.Errorf("resume: %s", md)
	}
	p.Boot = -1
	if _, _, err := Render(p); err == nil {
		t.Error("negative boot counter must be rejected")
	}
}

func TestRenderDesktopNeverLocksOrSuspends(t *testing.T) {
	// The agent user has no password: a locked screen cannot be unlocked, and a suspended VM does not wake up.
	ud, _, err := Render(base())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"lock-enabled=false", "idle-delay=uint32 0", "sleep-inactive-ac-type='nothing'",
		"system-db:local", "[ dconf, update ]",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("desktop user-data lacks %q", want)
		}
	}
	p := base()
	p.Profile = "headless"
	ud, _, _ = Render(p)
	if strings.Contains(ud, "dconf") {
		t.Error("a headless session has no GNOME and must not carry dconf settings")
	}
}

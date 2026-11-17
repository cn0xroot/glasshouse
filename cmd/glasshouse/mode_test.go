package main

import (
	"testing"

	"glasshouse/internal/policy"
)

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name                 string
		mode                 string
		gui, shell, headless bool
		want                 string
		wantErr              bool
	}{
		{name: "none", want: modeAuto},
		{name: "gui flag", gui: true, want: modeGUI},
		{name: "shell flag", shell: true, want: modeShell},
		{name: "legacy headless is shell", headless: true, want: modeShell},
		{name: "mode gui", mode: "gui", want: modeGUI},
		{name: "mode desktop alias", mode: "desktop", want: modeGUI},
		{name: "mode shell", mode: "shell", want: modeShell},
		{name: "shell and headless agree", shell: true, headless: true, want: modeShell},
		{name: "mode and flag agree", mode: "gui", gui: true, want: modeGUI},
		{name: "gui vs shell conflict", gui: true, shell: true, wantErr: true},
		{name: "gui vs headless conflict", gui: true, headless: true, wantErr: true},
		{name: "mode vs flag conflict", mode: "shell", gui: true, wantErr: true},
		{name: "unknown mode", mode: "vnc", wantErr: true},
	}
	for _, c := range cases {
		got, err := resolveMode(c.mode, c.gui, c.shell, c.headless)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCheckModePolicyNeverLoosens(t *testing.T) {
	desktop := policy.Default()
	desktop.VM.Profile = "desktop"
	headless := policy.Default()
	headless.VM.Profile = "headless"

	if err := checkModePolicy(modeGUI, desktop); err != nil {
		t.Errorf("gui on a desktop policy must be allowed: %v", err)
	}
	if err := checkModePolicy(modeGUI, headless); err == nil {
		t.Error("gui on a headless policy must be refused (flags cannot loosen the policy)")
	}
	for _, m := range []string{modeShell, modeAuto} {
		if err := checkModePolicy(m, headless); err != nil {
			t.Errorf("mode %q on a headless policy must be allowed: %v", m, err)
		}
	}
}

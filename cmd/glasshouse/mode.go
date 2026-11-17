package main

import (
	"fmt"

	"glasshouse/internal/policy"
)

// Launch modes for `glasshouse run`.
//
//	gui   - desktop VM (profile desktop); the desktop window opens once the session is ready.
//	shell - lightweight VM without a desktop (profile headless); drops into an agent shell in /workspace.
//	""    - not specified: follow the policy's vm.profile.
const (
	modeAuto  = ""
	modeGUI   = "gui"
	modeShell = "shell"
)

// resolveMode merges --mode, --gui, --shell and the legacy --headless flag into
// one launch mode. Conflicting requests are an error rather than a silent pick.
func resolveMode(mode string, gui, shell, headless bool) (string, error) {
	var want []string
	switch mode {
	case "":
	case "gui", "desktop":
		want = append(want, modeGUI)
	case "shell", "headless":
		want = append(want, modeShell)
	default:
		return "", fmt.Errorf("unknown --mode %q (want gui or shell)", mode)
	}
	if gui {
		want = append(want, modeGUI)
	}
	if shell || headless {
		want = append(want, modeShell)
	}
	for _, w := range want {
		if w != want[0] {
			return "", fmt.Errorf("--gui and --shell/--headless/--mode conflict: pick one launch mode")
		}
	}
	if len(want) == 0 {
		return modeAuto, nil
	}
	return want[0], nil
}

// checkModePolicy makes sure the requested mode does not loosen the policy: a
// policy (user or project) that already forces the headless profile cannot be
// turned back into a desktop session from the command line.
func checkModePolicy(mode string, pol policy.Policy) error {
	if mode == modeGUI && pol.VM.Profile != "desktop" {
		return fmt.Errorf("--gui requested, but the effective policy uses vm.profile %q; "+
			"a command-line flag cannot loosen the policy (edit your policy file, or remove the project's .glasshouse.yaml restriction)", pol.VM.Profile)
	}
	return nil
}

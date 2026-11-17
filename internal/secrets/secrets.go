// Package secrets resolves a policy key_ref to the secret itself, on the host.
// The resolved value only ever goes into the LLM gateway's upstream request.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Resolve supports:
//
//	env:NAME              environment variable of the glasshouse process
//	file:/path            first line of a file (must not be group/world readable)
//	keychain:SERVICE      macOS Keychain generic password
//	secret-service:NAME   Linux Secret Service (secret-tool lookup service NAME)
//
// Errors never contain the secret value.
func Resolve(ref string) (string, error) {
	scheme, arg, ok := strings.Cut(ref, ":")
	if !ok || arg == "" {
		return "", errors.New("key_ref must look like scheme:value (env:, file:, keychain:, secret-service:)")
	}
	switch scheme {
	case "env":
		v := strings.TrimSpace(os.Getenv(arg))
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", arg)
		}
		return v, nil
	case "file":
		st, err := os.Stat(arg)
		if err != nil {
			return "", err
		}
		if st.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s is readable by group/others; chmod 600 it", arg)
		}
		b, err := os.ReadFile(arg)
		if err != nil {
			return "", err
		}
		line, _, _ := strings.Cut(string(b), "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			return "", fmt.Errorf("%s is empty", arg)
		}
		return line, nil
	case "keychain":
		return run("security", "find-generic-password", "-s", arg, "-w")
	case "secret-service":
		return run("secret-tool", "lookup", "service", arg)
	}
	return "", fmt.Errorf("unknown key_ref scheme %q", scheme)
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s lookup failed: %w", name, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("%s returned an empty secret", name)
	}
	return v, nil
}

package miniyaml

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	src := `
version: 1
vm:
  profile: desktop   # comment
  cpus: 4
ssh:
  root: true
files:
  deny_patterns: [".env*", "*.pem", id_*]
network:
  allow:
    - github.com:443
    - "pypi.org:443"
credentials:
  llm_gateway:
    key_ref: keychain:glasshouse-anthropic
`
	m, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	vm := m["vm"].(map[string]any)
	if vm["profile"] != "desktop" || vm["cpus"] != int64(4) {
		t.Fatalf("vm: %#v", vm)
	}
	if m["ssh"].(map[string]any)["root"] != true {
		t.Fatal("ssh.root")
	}
	got := m["files"].(map[string]any)["deny_patterns"]
	if !reflect.DeepEqual(got, []any{".env*", "*.pem", "id_*"}) {
		t.Fatalf("deny_patterns: %#v", got)
	}
	allow := m["network"].(map[string]any)["allow"]
	if !reflect.DeepEqual(allow, []any{"github.com:443", "pypi.org:443"}) {
		t.Fatalf("allow: %#v", allow)
	}
	if m["credentials"].(map[string]any)["llm_gateway"].(map[string]any)["key_ref"] != "keychain:glasshouse-anthropic" {
		t.Fatal("key_ref")
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"tab":       "vm:\n\tcpus: 1\n",
		"dup":       "a: 1\na: 2\n",
		"inlinemap": "a: {b: 1}\n",
		"badindent": "a: 1\n   b: 2\n",
		"nocolon":   "just text\n",
		"unterm":    "a: \"x\n",
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

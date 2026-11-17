package netd

import (
	"fmt"
	"strconv"
	"strings"

	"glasshouse/internal/policy"
)

type allowEntry struct {
	host     string // exact host, or the suffix (".example.com") when wildcard
	wildcard bool
	port     int
}

type allowlist []allowEntry

func parseAllowlist(entries []string) (allowlist, error) {
	var out allowlist
	for _, e := range entries {
		if err := policy.ValidateAllowEntry(e); err != nil {
			return nil, fmt.Errorf("allow %q: %w", e, err)
		}
		i := strings.LastIndex(e, ":")
		port, _ := strconv.Atoi(e[i+1:])
		host := strings.ToLower(e[:i])
		if strings.HasPrefix(host, "*.") {
			out = append(out, allowEntry{host: host[1:], wildcard: true, port: port})
		} else {
			out = append(out, allowEntry{host: host, port: port})
		}
	}
	return out, nil
}

// match: exact host, or a wildcard that covers subdomains but never the apex.
func (a allowlist) match(host string, port int) bool {
	host = normHost(host)
	for _, e := range a {
		if e.port != port {
			continue
		}
		if !e.wildcard && host == e.host {
			return true
		}
		if e.wildcard && len(host) > len(e.host) && strings.HasSuffix(host, e.host) {
			return true
		}
	}
	return false
}

package cli

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// fetchBlocklist downloads an external blocklist and extracts the domains it
// blocks. It understands the common formats so popular public lists work
// out of the box: hosts files (`0.0.0.0 domain`), plain one-domain-per-line
// lists, and simple Adblock/uBlock network rules (`||domain^`). Comments
// (`#`, `!`) and non-domain lines are ignored.
func fetchBlocklist(url string) ([]string, error) {
	denies, _, err := fetchBlocklistFull(url)
	return denies, err
}

// fetchBlocklistFull fetches and parses url, returning denied domains and
// domain-wide exception allows (`@@||domain^` Adblock rules). Exceptions the
// destination-only ACL can't honor faithfully — path-limited or option-scoped
// rules — are dropped rather than over-allowed.
func fetchBlocklistFull(url string) (denies, allows []string, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	seenDeny := make(map[string]bool)
	seenAllow := make(map[string]bool)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "@@") {
			if d, ok := parseBlocklistExceptionLine(line); ok && !seenAllow[d] {
				seenAllow[d] = true
				allows = append(allows, d)
			}
			continue
		}
		for _, d := range parseBlocklistLine(line) {
			if !seenDeny[d] {
				seenDeny[d] = true
				denies = append(denies, d)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", url, err)
	}
	return denies, allows, nil
}

// parseBlocklistExceptionLine extracts the allowed domain from an Adblock
// exception rule (`@@||domain^`), or false if the line isn't one that maps
// onto a domain-wide allow. Only bare-domain exceptions qualify: a path
// (`/`), wildcard (`*`), mid-rule separator (`^`), or any rule option
// (`$domain=`, `$script`, ...) scopes the exception to specific requests,
// which a destination-only ACL cannot represent — those are skipped.
func parseBlocklistExceptionLine(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "@@||")
	if !ok {
		return "", false
	}
	if before, opts, found := strings.Cut(rest, "$"); found {
		if opts != "" {
			return "", false // request-scoped exception
		}
		rest = before
	}
	rest = strings.TrimSuffix(rest, "^")
	if strings.ContainsAny(rest, "/^*") || !isDomainish(rest) {
		return "", false
	}
	return rest, true
}

// parseBlocklistLine extracts the blocked domain(s) from one line of a
// hosts/plain/Adblock list, or nil if the line carries none.
func parseBlocklistLine(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
		return nil
	}
	// Adblock exception rule: never a block; parseBlocklistExceptionLine
	// decides whether it becomes an allow.
	if strings.HasPrefix(line, "@@") {
		return nil
	}
	// Adblock/uBlock network rule: ||example.com^  (ignore exception rules @@).
	if strings.HasPrefix(line, "||") {
		rest := strings.TrimPrefix(line, "||")
		if i := strings.IndexAny(rest, "^$/*"); i >= 0 {
			rest = rest[:i]
		}
		if isDomainish(rest) {
			return []string{rest}
		}
		return nil
	}
	if i := strings.IndexByte(line, '#'); i >= 0 { // strip trailing comment
		line = strings.TrimSpace(line[:i])
	}
	fields := strings.Fields(line)
	switch {
	case len(fields) == 0:
		return nil
	case len(fields) == 1: // plain domain list
		if isDomainish(fields[0]) {
			return []string{fields[0]}
		}
	default: // hosts format: <ip> <domain> [domain...]
		switch fields[0] {
		case "0.0.0.0", "127.0.0.1", "::1":
			var out []string
			for _, h := range fields[1:] {
				switch h {
				case "localhost", "localhost.localdomain", "broadcasthost", "ip6-localhost":
					continue
				}
				if isDomainish(h) {
					out = append(out, h)
				}
			}
			return out
		}
	}
	return nil
}

func isDomainish(s string) bool {
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	return !strings.ContainsAny(s, " \t/\\:")
}

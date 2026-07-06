package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseBlocklistExceptionLine(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{"@@||cdn.example.com^", "cdn.example.com", true},
		{"@@||cdn.example.com", "cdn.example.com", true},
		{"@@||cdn.example.com^$", "cdn.example.com", true}, // empty options suffix
		{"@@||x.com^$domain=y.com", "", false},             // scoped to a source domain
		{"@@||x.com^$script", "", false},                   // scoped to a request type
		{"@@||x.com/path", "", false},                      // path-limited
		{"@@||x.com^/sub", "", false},                      // separator mid-rule
		{"@@||x.*.com^", "", false},                        // wildcard
		{"@@|x.com^", "", false},                           // not a ||-anchored rule
		{"@@nonsense", "", false},
		{"||x.com^", "", false}, // plain block, not an exception
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := parseBlocklistExceptionLine(tt.line)
		require.Equal(t, tt.ok, ok, "parseBlocklistExceptionLine(%q) ok", tt.line)
		require.Equal(t, tt.want, got, "parseBlocklistExceptionLine(%q) domain", tt.line)
	}
}

func TestParseBlocklistLineSkipsExceptions(t *testing.T) {
	// Exception rules must never surface as blocks — not even malformed ones
	// that would otherwise pass the plain-domain heuristic.
	require.Nil(t, parseBlocklistLine("@@||cdn.example.com^"))
	require.Nil(t, parseBlocklistLine("@@||x.com^$domain=y.com"))
}

func TestFetchBlocklistFull(t *testing.T) {
	body := "! adblock header\n" +
		"# hosts comment\n" +
		"0.0.0.0 ads.example.com\n" +
		"0.0.0.0 localhost\n" +
		"tracker.example.com\n" +
		"||evil.example.com^\n" +
		"||evil.example.com^\n" + // duplicate block
		"@@||cdn.example.com^\n" +
		"@@||cdn.example.com^\n" + // duplicate exception
		"@@||x.com^$domain=y.com\n" + // scoped: skipped
		"@@||x.com/path\n" // path-limited: skipped
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	denies, allows, err := fetchBlocklistFull(srv.URL)
	require.NoError(t, err)
	require.Equal(t, []string{"ads.example.com", "tracker.example.com", "evil.example.com"}, denies)
	require.Equal(t, []string{"cdn.example.com"}, allows)

	// The denies-only wrapper sees the same file identically.
	got, err := fetchBlocklist(srv.URL)
	require.NoError(t, err)
	require.Equal(t, denies, got)
}

func TestFetchBlocklistFullHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := fetchBlocklistFull(srv.URL)
	require.Error(t, err)
}

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// `clawk auth` manages the long-lived Claude Code OAuth token clawk
// propagates into every sandbox. The motivation is the OAuth refresh-
// token race that hits anyone running more than one Claude Code session
// at a time: the rotating-refresh-token flow in ~/.claude/.credentials.json
// is single-use, and multiple sandboxes started from the same snapshot
// stampede the refresh endpoint at expiry — winner keeps a session,
// every loser gets invalid_grant and crashes back to /login.
//
// Switching to a `claude setup-token` 1-year token sidesteps the race
// entirely (the token is static — no refresh, no rotation). One copy on
// the host, propagated into every sandbox as CLAUDE_CODE_OAUTH_TOKEN.
//
// Anthropic ships `claude setup-token` to generate the token; we just
// store and propagate it. Issue refs: anthropics/claude-code#24317,
// #27933, #43392.

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authSetTokenCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authClearCmd)
}

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage the long-lived Claude Code OAuth token for sandboxes",
	Long: `clawk auth manages the single long-lived OAuth token clawk propagates
into every sandbox as CLAUDE_CODE_OAUTH_TOKEN.

Background: Claude Code's normal /login flow writes a rotating refresh
token to ~/.claude/.credentials.json. Each refresh invalidates the
previous one, so when N sandboxes start from the same snapshot and all
hit token expiry around the same time, only one survives — every other
sandbox crashes back to "Not logged in. Please run /login." This is a
known upstream race (anthropics/claude-code#24317, #43392).

A long-lived OAuth token, produced once by 'claude setup-token' on the
host, has no rotation — every sandbox can use the same value
indefinitely without coordination. clawk stores it at
~/.clawk/claude-oauth-token (mode 0600) and exports it via
/etc/profile.d/ in every sandbox it creates.

Typical setup:

  $ claude setup-token         # one-time, on the host. Prints a token.
  $ clawk auth set-token       # paste the token; clawk persists it.
  $ clawk                      # all future sandboxes use this token.

The token is "inference only" per Claude's docs — it authenticates
against your Pro/Max/Team subscription but cannot establish Remote
Control sessions. Bare-mode (claude --bare) does not read the token
env var; for that path use ANTHROPIC_API_KEY or an apiKeyHelper.`,
}

var authSetTokenCmd = &cobra.Command{
	Use:   "set-token [token]",
	Short: "Persist a long-lived OAuth token (overwrites any existing one)",
	Long: `Save a long-lived OAuth token at ~/.clawk/claude-oauth-token.

  $ clawk auth set-token <token>          # one-shot
  $ clawk auth set-token < token.txt      # pipe from a file
  $ clawk auth set-token                  # interactive (terminal echo off)

The token is stored with mode 0600 and written atomically (tmpfile +
rename) so an interrupted run can't leave an empty file. Generate the
token first by running 'claude setup-token' on the host — clawk does
not authenticate on your behalf.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAuthSetToken,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether an OAuth token is configured (and where it comes from)",
	RunE:  runAuthStatus,
}

var authClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Remove the persisted OAuth token",
	Long: `Delete ~/.clawk/claude-oauth-token. Removing a missing file is
not an error; new sandboxes will fall back to the rotating credentials
in the host Keychain / ~/.claude/.credentials.json — which means the
multi-sandbox race comes back. Only run clear if you intend to
re-authenticate from scratch.`,
	RunE: runAuthClear,
}

func runAuthSetToken(cmd *cobra.Command, args []string) error {
	root := clawkRoot()
	token, err := readTokenInput(cmd, args)
	if err != nil {
		return err
	}
	if err := sandbox.SaveOAuthToken(root, token); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"Saved token to %s (mode 0600).\n"+
			"New sandboxes will export CLAUDE_CODE_OAUTH_TOKEN automatically.\n"+
			"Run 'clawk auth status' to confirm.\n",
		sandbox.OAuthTokenPath(root))
	return nil
}

// readTokenInput resolves the token from (in order): a positional arg,
// non-tty stdin (pipe), or an interactive prompt with terminal echo off.
// We deliberately do NOT accept a --token=... flag — flags leak into
// shell history and `ps -ef` on multi-user systems. Arg form is still
// available for users who know the trade-off (e.g. CI bootstrap from
// a secrets manager); the prompt is the friendlier default.
func readTokenInput(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 1 {
		return strings.TrimSpace(args[0]), nil
	}

	stdin := int(os.Stdin.Fd())
	if !term.IsTerminal(stdin) {
		// Piped input — read everything to EOF and trim.
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", errors.New("empty token on stdin")
		}
		return tok, nil
	}

	fmt.Fprint(cmd.ErrOrStderr(),
		"Paste the token from `claude setup-token` (input hidden): ")
	raw, err := term.ReadPassword(stdin)
	// term.ReadPassword swallows the trailing newline — print one so the
	// next message starts on its own line.
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("reading token: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("no token provided")
	}
	return tok, nil
}

func runAuthStatus(cmd *cobra.Command, _ []string) error {
	root := clawkRoot()
	out := cmd.OutOrStdout()

	tokenPath := sandbox.OAuthTokenPath(root)
	token, src := sandbox.LoadOAuthToken(root)

	fmt.Fprintf(out, "Token file: %s\n", tokenPath)
	if fi, err := os.Stat(tokenPath); err == nil {
		fmt.Fprintf(out, "  exists:   yes (mode %o, %d bytes, modified %s)\n",
			fi.Mode().Perm(), fi.Size(), fi.ModTime().Format("2006-01-02 15:04:05"))
	} else {
		fmt.Fprintf(out, "  exists:   no\n")
	}

	// Don't print the token itself — show a fingerprint so the user can
	// tell two values apart without leaking the secret to terminal
	// history / over-the-shoulder.
	fmt.Fprintln(out)
	switch src {
	case sandbox.OAuthTokenSourceEnv:
		fmt.Fprintf(out,
			"Active source: CLAUDE_CODE_OAUTH_TOKEN env var (wins over file)\n"+
				"  fingerprint: %s\n", tokenFingerprint(token))
	case sandbox.OAuthTokenSourceFile:
		fmt.Fprintf(out,
			"Active source: file\n"+
				"  fingerprint: %s\n", tokenFingerprint(token))
	default:
		fmt.Fprintln(out,
			"Active source: none — new sandboxes will copy the host's rotating\n"+
				"               ~/.claude/.credentials.json blob (susceptible to the\n"+
				"               OAuth refresh-token race). Run 'clawk auth set-token'\n"+
				"               to fix.")
	}
	return nil
}

// tokenFingerprint returns a short prefix:suffix view of the token so
// `auth status` can prove which token is loaded without exposing the
// whole secret. 6 + 6 chars is enough to disambiguate the handful of
// tokens a user may rotate through.
func tokenFingerprint(tok string) string {
	if len(tok) <= 12 {
		return strings.Repeat("*", len(tok))
	}
	return tok[:6] + "…" + tok[len(tok)-6:]
}

func runAuthClear(cmd *cobra.Command, _ []string) error {
	root := clawkRoot()
	if err := sandbox.ClearOAuthToken(root); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"Cleared %s.\n", sandbox.OAuthTokenPath(root))
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: CLAUDE_CODE_OAUTH_TOKEN is still set in your environment "+
				"and will keep winning over the missing file until you unset it.")
	}
	return nil
}

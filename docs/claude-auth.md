# Sign in once: the Claude token

Claude Code's normal `/login` writes a *rotating* refresh token to
`~/.claude/.credentials.json`. Each refresh invalidates the previous one, so
when several sandboxes start from the same snapshot and hit token expiry around
the same time, only one survives — every other VM crashes back to
*"Not logged in. Please run /login."* (a known upstream race,
[claude-code#24317](https://github.com/anthropics/claude-code/issues/24317)).

The fix is a long-lived token: `claude setup-token` mints a 1-year OAuth token
that never rotates, so every sandbox can share one value with no coordination.
Generate it once on the host and hand it to clawk:

```sh
claude setup-token           # one-time, on the host — prints a token
clawk auth set-token         # paste it (input hidden); clawk persists it
clawk                        # every future sandbox is already signed in
```

clawk stores the token at `~/.clawk/claude-oauth-token` (mode `0600`) and
exports it into each sandbox as `CLAUDE_CODE_OAUTH_TOKEN` via `/etc/profile.d/`,
so Claude Code comes up authenticated with no `/login` prompt. An existing
`CLAUDE_CODE_OAUTH_TOKEN` in your host shell is picked up automatically and wins
over the stored file.

```sh
clawk auth status            # show whether a token is set, and its source
clawk auth set-token < tok   # pipe from a file (or pass as an arg) for scripting
clawk auth clear             # forget it; new sandboxes fall back to /login
```

The token is inference-only — it authenticates against your Pro/Max/Team
subscription but can't establish Remote Control sessions, and bare mode
(`claude --bare`) reads `ANTHROPIC_API_KEY` instead. Without a token, sandboxes
fall back to copying the host's rotating credentials and the multi-sandbox race
returns.

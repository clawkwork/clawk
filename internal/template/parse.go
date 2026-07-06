package template

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"time"
)

// FileSpec is one entry in a `files (...)` block: a host file copied
// into the guest at sandbox create and re-pushed on every `clawk up`.
// Snapshot semantics — edits on the host propagate when the user next
// runs `up`, not live. Use ShareSpec for files that rotate underneath
// (AWS STS, etc.).
//
//   - HostPath: host-side path. Tilde and $VAR are expanded at compose
//     time, not at parse time, because the parsing host (CI) and the
//     run host (laptop) may differ.
//   - GuestPath: absolute guest path. Empty falls back to HostPath
//     verbatim (with ~ resolved to the guest agent's $HOME).
//   - Mode: zero = preserve the host file's mode; non-zero overrides.
type FileSpec struct {
	HostPath  string
	GuestPath string
	Mode      fs.FileMode

	// Line/Col record where the entry appeared so duplicate-guest-path
	// conflicts can be reported back to the right line.
	Line, Col int
}

// ShareSpec is one entry in a `shares (...)` block: a host directory
// live-mounted into the guest via virtio-fs. Edits on the host are
// visible inside the guest without clawk involvement — the host owns
// the file lifecycle.
//
// Adding or removing a share requires `clawk down && clawk up` so the
// provider re-emits its virtio-fs device list. Disk state is preserved
// across that cycle; `clawk destroy` is not required.
type ShareSpec struct {
	HostPath  string
	GuestPath string // absolute guest mount point; empty = same as HostPath after tilde expansion
	ReadOnly  bool   // defaults to true at parse time (see parseSharesBlock)
	Line, Col int
}

// AgentDoc is one entry in an `agent (...)` block — a unit of persistent
// instructions or memory seed. Exactly one field is set: Text is inline
// markdown from a quoted string (one line); Path is a markdown file, relative
// to the clawk.mod directory, whose content is read at compose. The file form
// is how multi-line markdown is carried — its backticks, fences and quotes
// never have to survive the DSL's string lexing.
type AgentDoc struct {
	Text string
	Path string
}

// Template is the parsed form of a clawk.mod's `sandbox ( ... )` block.
//
// Directives are grouped: scalar VM settings live in `vm ( ... )` and egress
// policy in `network ( ... )`; `forwards` / `files` / `shares` / `skills` /
// `env` / `on <event>` sit at the block's top level. A sandbox block with
// `includes ( ... )` is a workspace root composing member repos; without it
// the block configures the repo it sits in.
//
// Callers (LoadWorkspace / LoadStandaloneClawkfile) decide which subset is
// valid for their context and reject out-of-place directives.
type Template struct {
	Name string // repo name override (defaults to dir basename)
	// SandboxName is the block-header name from the typed-block grammar
	// (`sandbox <name> ( ... )`, see ParseFileString). Empty when the header
	// omits it — the loaders fold a non-empty header into Name, so downstream
	// naming is one field.
	SandboxName string
	Provider    string   // e.g., "vz", "firecracker"; empty = default
	Includes    []string // repo paths the workspace composes
	Domains     []string // domain allow list additions
	IPs         []string // literal IP / CIDR allow list
	// Use lists named network policies referenced by `use <name> ...` inside
	// a `network ( ... )` block, in declaration order. Resolution against
	// `policy` blocks happens at compose time, not parse time.
	Use []string
	// DenyDomains and DenyIPs become guardrail denies in the sandbox's
	// "mod" policy block: they refuse the destination outright (a denied
	// domain covers its subdomains), override lower layers' allows, and
	// suppress the interactive prompt. See composeNetworkPolicy.
	DenyDomains []string
	DenyIPs     []string
	// DenySources are URLs of external blocklists (hosts files / EasyList /
	// plain domain lists, uBlock-style) fetched and parsed into denied domains
	// by the caller. Written as `deny source "<url>"` inside a `network ( … )`
	// block.
	DenySources []string
	Forwards    []string // port forward specs (PORT or HOST:GUEST)
	Env         []string // env var NAMES (not values) to pull from host and export in the VM

	// Lifecycle hooks. Each is a list of shell commands run inside the
	// VM at the named moment.
	//
	//   OnCreate runs once after first boot, before the runner attaches.
	//            Hard fails the up — see Sandbox.CreatePending.
	//   OnUp     runs every clawk-up after the VM is healthy.
	//   OnDown   runs every clawk-down before VM stop. (reserved; not yet wired)
	//   OnEnter  runs every clawk-run before the runner spawns. (reserved; not yet wired)
	OnCreate []string
	OnUp     []string
	OnDown   []string
	OnEnter  []string

	// Skills is the require-style list of Claude skills the project
	// assumes are available, identified by path (local or distributed).
	Skills []SkillRef

	// Files is the snapshot-on-up file list (`files (...)`).
	// See FileSpec for semantics.
	Files []FileSpec

	// Shares is the virtio-fs live-mount list (`shares (...)`).
	// See ShareSpec for semantics.
	Shares []ShareSpec

	// Instructions and Memory come from the `agent ( ... )` block: extra
	// persistent CLAUDE.md guidance and a baseline auto-memory seed. Each is
	// an ordered list of AgentDocs (inline text or a markdown file) resolved
	// to content at compose, where the namespace's equivalents layer ahead.
	Instructions []AgentDoc
	Memory       []AgentDoc

	// Nested enables hardware nested virtualization for the sandbox.
	// Bare directive: present and true, absent and false. There's no
	// `nested false` form — profile overlays cannot un-set.
	Nested bool

	// CPU is the vCPU count exposed to the guest. Zero = provider default.
	// VZ and KVM don't reserve host CPU time for idle guest vCPUs, so this is
	// effectively a burst ceiling rather than a reservation.
	CPU uint

	// MemoryMiB is the baseline memory target in mebibytes. When set together
	// with MemoryMaxMiB > MemoryMiB, the provider configures a virtio-balloon
	// that reclaims (max - baseline) back to the host at boot and deflates on
	// guest pressure (deflate_on_oom). Zero = follow MemoryMaxMiB (no balloon).
	MemoryMiB uint64

	// MemoryMaxMiB is the hard cap on guest memory in mebibytes — the amount
	// the guest sees at boot. Zero = provider default.
	MemoryMaxMiB uint64

	// IdleTimeoutSec is the sandbox's idle-stop timeout in seconds: how
	// long the VM may sit with no attached session and a quiescent guest
	// before the daemon stops it to reclaim host memory. Declared as
	// `idle_timeout <dur|off>` inside the `vm ( ... )` block. Zero = unset
	// (the provider default applies); negative = never stop ("off").
	IdleTimeoutSec int64

	// Image is an OCI image reference (e.g. "golang:1.25") the sandbox
	// boots as its root filesystem. Declared as `image <ref>` inside the
	// `vm ( ... )` block. Empty = the built-in clawk-dev default.
	Image string

	// Kernel overrides the guest kernel the vz provider direct-boots: a
	// local vmlinux path or an http(s) URL. Declared as `kernel <path|url>`
	// inside the `vm ( ... )` block. Empty = the default Kata kernel.
	Kernel string
}

// Merge folds the directives of `over` on top of `t`, mutating t in place.
// This is the overlay semantics used by profiles: the base file declares
// defaults, the profile file adds more. Scalars in `over` win only when they
// are non-empty; lists are unioned (appended, duplicates removed later by
// the sandbox-composition step).
//
// We intentionally do NOT allow profiles to shrink the config: there's no
// way to say "remove this allowed domain". That keeps profiles analysable —
// a reviewer sees only additions relative to the base.
func (t *Template) Merge(over *Template) {
	if over == nil {
		return
	}
	if over.Name != "" {
		t.Name = over.Name
	}
	if over.SandboxName != "" {
		t.SandboxName = over.SandboxName
	}
	if over.Provider != "" {
		t.Provider = over.Provider
	}
	if over.Kernel != "" {
		t.Kernel = over.Kernel
	}
	t.Includes = append(t.Includes, over.Includes...)
	t.Domains = append(t.Domains, over.Domains...)
	t.IPs = append(t.IPs, over.IPs...)
	t.DenyDomains = append(t.DenyDomains, over.DenyDomains...)
	t.DenyIPs = append(t.DenyIPs, over.DenyIPs...)
	t.Use = append(t.Use, over.Use...)
	t.Forwards = append(t.Forwards, over.Forwards...)
	t.Env = append(t.Env, over.Env...)
	t.OnCreate = append(t.OnCreate, over.OnCreate...)
	t.OnUp = append(t.OnUp, over.OnUp...)
	t.OnDown = append(t.OnDown, over.OnDown...)
	t.OnEnter = append(t.OnEnter, over.OnEnter...)
	t.Skills = mergeSkills(t.Skills, over.Skills)
	t.Files = append(t.Files, over.Files...)
	t.Shares = append(t.Shares, over.Shares...)
	t.Instructions = append(t.Instructions, over.Instructions...)
	t.Memory = append(t.Memory, over.Memory...)
	// Bool fields: OR so a profile can only enable, never disable.
	if over.Nested {
		t.Nested = true
	}
	// Scalar resource fields: profile value wins only if set (non-zero).
	// Same rule as Provider/Name — a profile can raise the limit, base
	// file's value stays put if the profile omits it.
	if over.CPU != 0 {
		t.CPU = over.CPU
	}
	if over.MemoryMiB != 0 {
		t.MemoryMiB = over.MemoryMiB
	}
	if over.MemoryMaxMiB != 0 {
		t.MemoryMaxMiB = over.MemoryMaxMiB
	}
	if over.IdleTimeoutSec != 0 {
		t.IdleTimeoutSec = over.IdleTimeoutSec
	}
}

type parser struct {
	toks []Token
	i    int
}

func (p *parser) peek() Token { return p.toks[p.i] }
func (p *parser) advance() {
	if p.i < len(p.toks)-1 {
		p.i++
	}
}

func (p *parser) errorAt(t Token, format string, args ...any) error {
	return fmt.Errorf("line %d col %d: "+format, append([]any{t.Line, t.Col}, args...)...)
}

// skipNewlines consumes zero or more consecutive NEWLINE tokens.
func (p *parser) skipNewlines() {
	for p.peek().Kind == TokNewline {
		p.advance()
	}
}

// expectNewlineOrEOF consumes a NEWLINE (or tolerates EOF at end of file).
// Statement terminators are newlines; requiring them catches accidental
// merged statements and keeps error messages pointing at the right line.
func (p *parser) expectNewlineOrEOF() error {
	t := p.peek()
	switch t.Kind {
	case TokNewline:
		p.advance()
		return nil
	case TokEOF:
		return nil
	}
	return p.errorAt(t, "expected newline, got %s", t)
}

// parseTemplateDirective dispatches one directive of the `sandbox ( ... )`
// block body (parseSandboxBlock); t is the current token, already checked
// to be a TokIdent by the caller.
func (p *parser) parseTemplateDirective(tmpl *Template, t Token) error {
	switch t.Val {
	case "includes":
		return p.parseIdentBlock(&tmpl.Includes, "includes")
	case "vm":
		return p.parseVMBlock(tmpl)
	case "network":
		return p.parseNetworkBlock(tmpl)
	case "skills":
		return p.parseSkillsBlock(tmpl)
	case "on":
		return p.parseOnDirective(tmpl)
	case "forwards":
		return p.parseIdentBlock(&tmpl.Forwards, "forwards")
	case "files":
		return p.parseFilesBlock(tmpl)
	case "shares":
		return p.parseSharesBlock(tmpl)
	case "agent":
		return p.parseAgentBlock(tmpl)
	case "env":
		// env ( FOO BAR BAZ )  or  env FOO
		// Values come from the host shell at `clawk run / up` time;
		// the Clawkfile only names which variables the project needs.
		// Never persisted as values — avoids checking secrets into repo
		// config. Names must be shell-variable shaped (validateEnvName).
		return p.parseValidatedIdentBlock(&tmpl.Env, "env", validateEnvName)
	case "apps", "app":
		// Previous iteration's directives — explicitly rejected so users
		// migrating from an older Clawkfile get a clear message pointing
		// at the new model.
		return p.errorAt(t,
			"%q is no longer supported; use 'includes' in the workspace "+
				"and a single Clawkfile per repo", t.Val)
	case "repos":
		// The legacy single-file template's repo list — retired with that
		// format. Rejected by name so a surviving file gets a rename hint
		// instead of the generic unknown-directive error.
		return p.errorAt(t,
			"'repos' was replaced by 'includes'; list repositories with "+
				"'includes ( ... )' in the workspace sandbox block")
	default:
		return p.errorAt(t,
			"unknown directive %q (want %s)", t.Val,
			describeFirst([]string{
				"vm", "network", "forwards", "files", "shares",
				"agent", "skills", "on", "includes", "env",
			}))
	}
}

// parseIdentBlock handles `directive ( IDENT ... )` OR `directive IDENT` as
// a single inline entry. Used for includes, forwards, and env.
//
// Go's go.mod uses the same two-form syntax (`require foo v1` vs
// `require ( foo v1 \n bar v2 )`) and it reads well — one-liners stay short,
// lists use the block form.
func (p *parser) parseIdentBlock(dst *[]string, directive string) error {
	return p.parseValidatedIdentBlock(dst, directive, nil)
}

// parseValidatedIdentBlock is parseIdentBlock with a per-entry validate
// hook, run at the entry's token so a bad entry is reported on its own
// line. nil validate accepts anything ident-shaped.
func (p *parser) parseValidatedIdentBlock(dst *[]string, directive string, validate func(string) error) error {
	p.advance() // consume directive name
	t := p.peek()

	// Inline form: `directive IDENT`
	if t.Kind == TokIdent {
		if isKeyword(t.Val) {
			return p.errorAt(t, "unexpected keyword %q in %q", t.Val, directive)
		}
		if validate != nil {
			if err := validate(t.Val); err != nil {
				return p.errorAt(t, "%v", err)
			}
		}
		*dst = append(*dst, t.Val)
		p.advance()
		return p.expectNewlineOrEOF()
	}

	// Block form: `directive ( ... )`
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' or identifier after %q, got %s", directive, t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected entry or ')' in %q, got %s", directive, t)
		}
		if isKeyword(t.Val) {
			return p.errorAt(t, "unexpected keyword %q in %q", t.Val, directive)
		}
		if validate != nil {
			if err := validate(t.Val); err != nil {
				return p.errorAt(t, "%v", err)
			}
		}
		*dst = append(*dst, t.Val)
		p.advance()
	}
}

// validateEnvName enforces the POSIX shell variable-name shape on `env`
// entries. Deliberately NOT uppercase-only: lowercase names like
// http_proxy are legitimate. Enforced at parse time because an invalid
// name written here would otherwise fail much later, inside the guest's
// generated profile script, where the error is far harder to trace.
func validateEnvName(s string) error {
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("invalid env name %q: must not start with a digit", s)
			}
		default:
			return fmt.Errorf("invalid env name %q: names only (letters, digits, '_'), values come from the host shell", s)
		}
	}
	return nil
}

func (p *parser) parseProvider(tmpl *Template) error {
	p.advance() // consume "provider"
	val := p.peek()
	if val.Kind != TokIdent {
		return p.errorAt(val, "expected provider name, got %s", val)
	}
	if isKeyword(val.Val) {
		return p.errorAt(val, "provider name %q is a reserved keyword", val.Val)
	}
	if tmpl.Provider != "" {
		return p.errorAt(val, "duplicate 'provider' directive")
	}
	tmpl.Provider = val.Val
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseCPU handles `cpu <N>` where N is a positive integer (vCPU count).
func (p *parser) parseCPU(tmpl *Template) error {
	p.advance() // consume "cpu"
	val := p.peek()
	if val.Kind != TokIdent {
		return p.errorAt(val, "expected vCPU count after 'cpu', got %s", val)
	}
	if tmpl.CPU != 0 {
		return p.errorAt(val, "duplicate 'cpu' directive")
	}
	n, err := strconv.ParseUint(val.Val, 10, 32)
	if err != nil {
		return p.errorAt(val, "invalid cpu count %q: %v", val.Val, err)
	}
	if n == 0 {
		return p.errorAt(val, "cpu count must be >= 1, got %q", val.Val)
	}
	tmpl.CPU = uint(n)
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseMemory handles `memory <size>` and `memory_max <size>` — a single
// size value in MiB, GiB, TiB (or SI MB/GB/TB). Bare numbers without a
// unit suffix are rejected to force unit-explicit configs.
func (p *parser) parseMemory(dst *uint64, directive string) error {
	p.advance() // consume directive name
	val := p.peek()
	if val.Kind != TokIdent {
		return p.errorAt(val, "expected size after %q, got %s", directive, val)
	}
	if *dst != 0 {
		return p.errorAt(val, "duplicate %q directive", directive)
	}
	mib, err := parseMiB(val.Val)
	if err != nil {
		return p.errorAt(val, "%q: %v", directive, err)
	}
	*dst = mib
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseMiB parses a memory size literal into mebibytes. Accepts IEC units
// (MiB, GiB, TiB) and their shorthands (M, G, T), and SI units
// (MB, GB, TB). Plain numbers without a unit are rejected.
func parseMiB(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q has no numeric prefix", s)
	}
	if i == len(s) {
		return 0, fmt.Errorf("size %q needs a unit suffix (MiB, GiB, TiB)", s)
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	unit := s[i:]
	const (
		mib = uint64(1)
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch unit {
	case "MiB", "M":
		return n * mib, nil
	case "GiB", "G":
		return n * gib, nil
	case "TiB", "T":
		return n * tib, nil
	case "MB":
		return n * 1_000_000 / (1 << 20), nil
	case "GB":
		return n * 1_000_000_000 / (1 << 20), nil
	case "TB":
		return n * 1_000_000_000_000 / (1 << 20), nil
	}
	return 0, fmt.Errorf("size %q: unknown unit %q (want MiB, GiB, TiB)", s, unit)
}

// parseIdleTimeout handles `idle_timeout <dur|off>` inside the vm block.
// Durations use Go syntax ("30m", "1h30m"); "off" and "0" disable the
// idle stop entirely (stored as -1 so unset stays distinguishable).
// Sub-minute timeouts are rejected — an idle check runs on a coarse tick
// and a seconds-scale timeout would just flap the VM.
func (p *parser) parseIdleTimeout(tmpl *Template) error {
	p.advance() // consume "idle_timeout"
	val := p.peek()
	if val.Kind != TokIdent {
		return p.errorAt(val, "expected duration or 'off' after 'idle_timeout', got %s", val)
	}
	if tmpl.IdleTimeoutSec != 0 {
		return p.errorAt(val, "duplicate 'idle_timeout' directive")
	}
	switch val.Val {
	case "off", "0":
		tmpl.IdleTimeoutSec = -1
	default:
		d, err := time.ParseDuration(val.Val)
		if err != nil {
			return p.errorAt(val, "invalid idle_timeout %q: want a duration like 30m or 'off'", val.Val)
		}
		if d < time.Minute {
			return p.errorAt(val, "idle_timeout %q too short: minimum 1m (use 'off' to disable)", val.Val)
		}
		tmpl.IdleTimeoutSec = int64(d / time.Second)
	}
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseNested handles the bare `nested` directive. It takes no value —
// setting Template.Nested true and requiring the line to end immediately
// keeps the syntax unambiguous and matches how Go's `go 1.x` line works
// (a keyword followed by end-of-line).
func (p *parser) parseNested(tmpl *Template) error {
	p.advance() // consume "nested"
	tmpl.Nested = true
	return p.expectNewlineOrEOF()
}

// parseVMBlock handles the `vm ( ... )` block — the scalar VM settings:
// provider, cpu, memory, memory_max, nested, image, kernel. Each reuses a
// dedicated per-directive parser so size-unit handling and duplicate
// detection have a single source of truth.
func (p *parser) parseVMBlock(tmpl *Template) error {
	p.advance() // consume "vm"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'vm', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected vm directive or ')', got %s", t)
		}
		switch t.Val {
		case "provider":
			if err := p.parseProvider(tmpl); err != nil {
				return err
			}
		case "cpu":
			if err := p.parseCPU(tmpl); err != nil {
				return err
			}
		case "memory":
			if err := p.parseMemory(&tmpl.MemoryMiB, "memory"); err != nil {
				return err
			}
		case "memory_max":
			if err := p.parseMemory(&tmpl.MemoryMaxMiB, "memory_max"); err != nil {
				return err
			}
		case "nested":
			if err := p.parseNested(tmpl); err != nil {
				return err
			}
		case "idle_timeout":
			if err := p.parseIdleTimeout(tmpl); err != nil {
				return err
			}
		case "image":
			if err := p.parseImage(tmpl); err != nil {
				return err
			}
		case "kernel":
			if err := p.parseKernel(tmpl); err != nil {
				return err
			}
		default:
			return p.errorAt(t,
				"unknown 'vm' directive %q (want %s)", t.Val,
				describeFirst([]string{
					"provider", "cpu", "memory", "memory_max", "nested", "idle_timeout", "image", "kernel",
				}))
		}
	}
}

// parseImage handles `image <ref>` inside the vm block — the OCI image
// reference the sandbox boots as its root filesystem. References like
// "golang:1.25" or "ghcr.io/org/img:tag" lex as a single ident; quoted
// strings are accepted too for anything the lexer would split.
func (p *parser) parseImage(tmpl *Template) error {
	p.advance() // consume "image"
	val := p.peek()
	if val.Kind != TokIdent && val.Kind != TokString {
		return p.errorAt(val, "expected image reference after 'image', got %s", val)
	}
	if val.Kind == TokIdent && isKeyword(val.Val) {
		return p.errorAt(val, "image reference %q is a reserved keyword", val.Val)
	}
	if tmpl.Image != "" {
		return p.errorAt(val, "duplicate 'image' directive")
	}
	tmpl.Image = val.Val
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseKernel handles `kernel <path|url>` inside the vm block — a guest
// kernel override (local vmlinux path or http(s) URL) for direct-kernel
// boot. Same lexing as parseImage: paths/URLs lex as a single ident, and
// quoted strings are accepted for anything the lexer would split.
func (p *parser) parseKernel(tmpl *Template) error {
	p.advance() // consume "kernel"
	val := p.peek()
	if val.Kind != TokIdent && val.Kind != TokString {
		return p.errorAt(val, "expected kernel path or URL after 'kernel', got %s", val)
	}
	if val.Kind == TokIdent && isKeyword(val.Val) {
		return p.errorAt(val, "kernel reference %q is a reserved keyword", val.Val)
	}
	if tmpl.Kernel != "" {
		return p.errorAt(val, "duplicate 'kernel' directive")
	}
	tmpl.Kernel = val.Val
	p.advance()
	return p.expectNewlineOrEOF()
}

// parseNetworkBlock handles `network ( use ... | allow ... | deny ... )`.
// Each entry starts with `use`, `allow` or `deny`, then policy names, a
// bare domain, or `ip <ADDR>`. Domains and IPs accumulate into Domains/IPs
// (allow) or DenyDomains/DenyIPs (deny); the compose step folds them into
// the sandbox's "mod" policy block, where denies are guardrails.
func (p *parser) parseNetworkBlock(tmpl *Template) error {
	p.advance() // consume "network"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'network', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected 'allow' or 'deny' or ')', got %s", t)
		}
		switch t.Val {
		case "allow":
			p.advance()
			if err := p.parseNetworkEntry(tmpl, false); err != nil {
				return err
			}
		case "deny":
			p.advance()
			if err := p.parseNetworkEntry(tmpl, true); err != nil {
				return err
			}
		case "use":
			if err := p.parseUseEntry(tmpl); err != nil {
				return err
			}
		default:
			return p.errorAt(t,
				"unknown 'network' directive %q (want 'allow', 'deny' or 'use')", t.Val)
		}
	}
}

// parseUseEntry handles `use <name> [<name>...]` inside a network block —
// one or more bare policy names accumulated into Template.Use in declaration
// order. Duplicates (within the file, across all `use` lines) are rejected:
// the reference list is a set, and a repeated name is almost always a typo'd
// second policy rather than an intentional restatement.
func (p *parser) parseUseEntry(tmpl *Template) error {
	p.advance() // consume "use"
	if t := p.peek(); t.Kind != TokIdent {
		return p.errorAt(t, "expected policy name after 'use', got %s", t)
	}
	for p.peek().Kind == TokIdent {
		t := p.peek()
		if isKeyword(t.Val) {
			return p.errorAt(t, "unexpected keyword %q in 'use'", t.Val)
		}
		if slices.Contains(tmpl.Use, t.Val) {
			return p.errorAt(t, "duplicate policy %q in 'use'", t.Val)
		}
		tmpl.Use = append(tmpl.Use, t.Val)
		p.advance()
	}
	return nil
}

// parseNetworkEntry reads the value half of a network block entry — bare
// domain or `ip <ADDR>` — into the allow or deny slice depending on `deny`.
func (p *parser) parseNetworkEntry(tmpl *Template, deny bool) error {
	t := p.peek()
	if t.Kind != TokIdent {
		return p.errorAt(t, "expected domain or 'ip', got %s", t)
	}
	if t.Val == "ip" {
		p.advance()
		ip := p.peek()
		if ip.Kind != TokIdent {
			return p.errorAt(ip, "expected IP or CIDR after 'ip', got %s", ip)
		}
		if deny {
			tmpl.DenyIPs = append(tmpl.DenyIPs, ip.Val)
		} else {
			tmpl.IPs = append(tmpl.IPs, ip.Val)
		}
		p.advance()
		return nil
	}
	// `deny source "<url>"` — an external blocklist (uBlock/hosts/EasyList),
	// fetched and expanded into denied domains by the caller at apply time.
	if deny && t.Val == "source" {
		p.advance()
		u := p.peek()
		if u.Kind != TokString {
			return p.errorAt(u, "expected a quoted URL after 'deny source', got %s", u)
		}
		tmpl.DenySources = append(tmpl.DenySources, u.Val)
		p.advance()
		return nil
	}
	if isKeyword(t.Val) {
		return p.errorAt(t, "unexpected keyword %q in network entry", t.Val)
	}
	if deny {
		tmpl.DenyDomains = append(tmpl.DenyDomains, t.Val)
	} else {
		tmpl.Domains = append(tmpl.Domains, t.Val)
	}
	p.advance()
	return nil
}

// parseSkillsBlock handles `skills ( <path> [<version>] ... )`. Each entry
// is a path-shaped identifier with an optional version on the same line.
// Paths are classified eagerly (local-home / local-workspace / distributed);
// versions are validated against the tidy-input rules but not resolved.
func (p *parser) parseSkillsBlock(tmpl *Template) error {
	p.advance() // consume "skills"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'skills', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected skill path or ')', got %s", t)
		}
		ref := SkillRef{
			Path: t.Val,
			Kind: ClassifySkillPath(t.Val),
			Line: t.Line,
			Col:  t.Col,
		}
		p.advance()
		// Optional version on the same line (must precede a newline).
		if v := p.peek(); v.Kind == TokIdent {
			ref.Version = v.Val
			ref.VersionLine = v.Line
			ref.VersionCol = v.Col
			p.advance()
		}
		if err := validateSkillRef(ref); err != nil {
			return p.errorAt(t, "%v", err)
		}
		tmpl.Skills = append(tmpl.Skills, ref)
		// A skill entry must end at end-of-line (or block close); reject
		// trailing tokens to keep error messages anchored to the right line.
		if next := p.peek(); next.Kind != TokNewline && next.Kind != TokRParen && next.Kind != TokEOF {
			return p.errorAt(next,
				"unexpected token after skill entry; one path and optional version per line")
		}
	}
}

// parseFilesBlock handles `files ( <host> [<guest>] [<mode>] ... )`.
// Each entry is up to three whitespace-separated tokens on one line:
//
//	~/.kube/config_staging_only
//	~/.kube/config_staging_only  /home/agent/k8s/config
//	~/.kube/config_staging_only  /home/agent/k8s/config  0600
//
// A token that matches octal mode syntax (^0[0-7]+$) is interpreted as
// the mode. Anything else is treated as a path. The grammar accepts the
// two-token form `<host> <mode>` so users can override the mode without
// repeating the host path as the guest path.
func (p *parser) parseFilesBlock(tmpl *Template) error {
	p.advance() // consume "files"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'files', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected file path or ')', got %s", t)
		}
		spec := FileSpec{HostPath: t.Val, Line: t.Line, Col: t.Col}
		p.advance()
		// Optional second and third tokens on the same line; classify by
		// shape so users don't have to write a placeholder.
		for range 2 {
			nx := p.peek()
			if nx.Kind != TokIdent {
				break
			}
			if mode, ok := parseFileMode(nx.Val); ok {
				if spec.Mode != 0 {
					return p.errorAt(nx, "duplicate mode on files entry")
				}
				spec.Mode = mode
				p.advance()
				continue
			}
			if spec.GuestPath != "" {
				return p.errorAt(nx, "unexpected token after files entry; one host and optional guest path per line")
			}
			spec.GuestPath = nx.Val
			p.advance()
		}
		if next := p.peek(); next.Kind != TokNewline && next.Kind != TokRParen && next.Kind != TokEOF {
			return p.errorAt(next,
				"unexpected token after files entry; one host and optional guest path per line")
		}
		tmpl.Files = append(tmpl.Files, spec)
	}
}

// parseSharesBlock handles `shares ( <host> [<guest>] [ro|rw] ... )`.
// Mount points default to ReadOnly = true: the host owns rotation for
// the use cases we built this for (`~/.aws`), and an accidental in-VM
// write to a shared credential file is exactly the failure mode the
// default should prevent. Users opt into `rw` per line when they need it.
func (p *parser) parseSharesBlock(tmpl *Template) error {
	p.advance() // consume "shares"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'shares', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected share path or ')', got %s", t)
		}
		spec := ShareSpec{HostPath: t.Val, ReadOnly: true, Line: t.Line, Col: t.Col}
		p.advance()
		var sawFlag bool
		for range 2 {
			nx := p.peek()
			if nx.Kind != TokIdent {
				break
			}
			switch nx.Val {
			case "ro":
				if sawFlag {
					return p.errorAt(nx, "duplicate ro/rw flag on shares entry")
				}
				spec.ReadOnly = true
				sawFlag = true
				p.advance()
			case "rw":
				if sawFlag {
					return p.errorAt(nx, "duplicate ro/rw flag on shares entry")
				}
				spec.ReadOnly = false
				sawFlag = true
				p.advance()
			default:
				if spec.GuestPath != "" {
					return p.errorAt(nx, "unexpected token after shares entry; one host and optional guest path per line")
				}
				spec.GuestPath = nx.Val
				p.advance()
			}
		}
		if next := p.peek(); next.Kind != TokNewline && next.Kind != TokRParen && next.Kind != TokEOF {
			return p.errorAt(next,
				"unexpected token after shares entry; one host and optional guest path per line")
		}
		tmpl.Shares = append(tmpl.Shares, spec)
	}
}

// parseAgentBlock handles `agent ( instructions ... | memory "..." )` — the
// persistent agent context seeded into a sandbox. `instructions` accumulates
// markdown guidance blocks (inline string or a parenthesised list); `memory`
// is a single baseline auto-memory document. Both take quoted strings, so
// markdown punctuation never collides with the DSL grammar.
func (p *parser) parseAgentBlock(tmpl *Template) error {
	p.advance() // consume "agent"
	t := p.peek()
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(' after 'agent', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return p.errorAt(t, "expected agent directive or ')', got %s", t)
		}
		switch t.Val {
		case "instructions":
			if err := p.parseAgentDocs(&tmpl.Instructions, "instructions"); err != nil {
				return err
			}
		case "memory":
			if err := p.parseAgentDocs(&tmpl.Memory, "memory"); err != nil {
				return err
			}
		default:
			return p.errorAt(t,
				"unknown 'agent' directive %q (want %s)", t.Val,
				describeFirst([]string{"instructions", "memory"}))
		}
	}
}

// parseAgentDocs reads `directive <value>` or `directive ( <value> ... )`
// where each value is EITHER a quoted string (inline markdown, one line) OR a
// bare path (a markdown file read at compose). A file is the way to carry
// multi-line markdown: its backticks, fences and quotes never enter the DSL.
// Order is preserved across mixed entries.
func (p *parser) parseAgentDocs(dst *[]AgentDoc, directive string) error {
	p.advance() // consume directive name
	t := p.peek()
	// Inline single value: a string (inline text) or a bare path.
	if t.Kind == TokString || t.Kind == TokIdent {
		*dst = append(*dst, agentDoc(t))
		p.advance()
		return p.expectNewlineOrEOF()
	}
	if t.Kind != TokLParen {
		return p.errorAt(t, "expected '(', a path, or a string after %q, got %s", directive, t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokString && t.Kind != TokIdent {
			return p.errorAt(t, "expected a path, a string, or ')' in %q, got %s", directive, t)
		}
		*dst = append(*dst, agentDoc(t))
		p.advance()
	}
}

// agentDoc classifies a value token: a quoted string is inline markdown, a
// bare token is a path to a markdown file resolved against the clawk.mod dir.
func agentDoc(t Token) AgentDoc {
	if t.Kind == TokString {
		return AgentDoc{Text: t.Val}
	}
	return AgentDoc{Path: t.Val}
}

// parseFileMode recognises a token of the form `0NNN` (e.g. `0600`,
// `0755`) as an octal file mode. Returns ok=false for anything that
// doesn't match the shape, leaving classification to the caller.
//
// The leading-zero requirement is deliberate: `600` alone is ambiguous
// (is the user thinking decimal 600 or octal 0600?), and forcing the
// `0` prefix matches both the strconv.ParseUint base-0 idiom and the
// way every unix mode is written in user-facing docs.
func parseFileMode(s string) (fs.FileMode, bool) {
	if len(s) < 2 || s[0] != '0' {
		return 0, false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '7' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, false
	}
	return fs.FileMode(v), true
}

// parseOnDirective handles `on <event> ( "cmd" ... )`. Recognised events:
// create, up, down, enter. Each maps to a separate command list on Template;
// the runtime treats them differently (see Sandbox.CreatePending semantics).
func (p *parser) parseOnDirective(tmpl *Template) error {
	p.advance() // consume "on"
	ev := p.peek()
	if ev.Kind != TokIdent {
		return p.errorAt(ev, "expected event name after 'on', got %s", ev)
	}
	var dst *[]string
	switch ev.Val {
	case "create":
		dst = &tmpl.OnCreate
	case "up":
		dst = &tmpl.OnUp
	case "down":
		dst = &tmpl.OnDown
	case "enter":
		dst = &tmpl.OnEnter
	default:
		return p.errorAt(ev,
			"unknown 'on' event %q (want %s)", ev.Val,
			describeFirst([]string{"create", "up", "down", "enter"}))
	}
	p.advance() // consume event name
	// Accept the same two shapes as the list directives: an inline single
	// string (`on up "cmd"`) or a parenthesised block of strings.
	t := p.peek()
	if t.Kind == TokString {
		// Inline form: `on up "cmd"`.
		*dst = append(*dst, t.Val)
		p.advance()
		return p.expectNewlineOrEOF()
	}
	if t.Kind != TokLParen {
		return p.errorAt(t,
			"expected '(' or string after 'on %s', got %s", ev.Val, t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return p.expectNewlineOrEOF()
		}
		if t.Kind != TokString {
			return p.errorAt(t,
				"expected string or ')' in 'on %s', got %s", ev.Val, t)
		}
		*dst = append(*dst, t.Val)
		p.advance()
	}
}

// mergeSkills unions two skill lists by Path, keeping the first occurrence's
// Version. Profile overlays therefore can ADD a skill but cannot reassign a
// version — that matches the broader "profiles only widen" rule used by
// Merge for other slices.
func mergeSkills(base, over []SkillRef) []SkillRef {
	if len(over) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s.Path] = true
	}
	out := append([]SkillRef(nil), base...)
	for _, s := range over {
		if seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		out = append(out, s)
	}
	return out
}

package template

import (
	"strconv"
	"time"
)

// FormatVersion is the newest clawk.mod format version this parser
// understands. A file may declare the version it requires with a top-level
// `clawk <n>` directive; a file without one is version 1. Bump this only
// for changes an older parser would silently misread — purely additive
// directives don't need a bump, because older parsers already reject
// unknown directives by name with a clear error.
const FormatVersion = 1

// File is the parsed form of a typed-block clawk file: a sequence of
// `<kind> [<name>] ( ... )` blocks with kinds sandbox, policy and namespace.
// There is no implicit root document and no flat top-level directives — a
// clawk.mod IS its block list. See ParseFileString for the migration story
// away from the legacy flat grammar.
type File struct {
	Sandbox    *Template // nil when the file defines no sandbox
	Policies   []PolicyDef
	Namespaces []NamespaceDef

	// FormatVersion is the value of the file's `clawk <n>` directive;
	// zero when the directive is absent (which means version 1). Files
	// requiring a version newer than FormatVersion fail at parse with an
	// upgrade hint, so a future format change degrades into a readable
	// error on old clawks instead of silent misparsing.
	FormatVersion int
}

// PolicyDef is one `policy <name> ( ... )` block: a named, reusable network
// egress policy referenced from network blocks via `use <name>`. Resolution
// of references happens at compose time, not parse time — a clawk file may
// use a policy defined elsewhere (e.g. registered by `clawk apply`).
type PolicyDef struct {
	Name string // required for policy blocks

	// Allow/Deny lists use the same entry grammar as a `network ( ... )`
	// block: bare domains and `ip <ADDR>` literals/CIDRs.
	AllowDomains, AllowIPs, DenyDomains, DenyIPs []string

	// Sources are URLs of external blocklists (`source "<url>"` entries),
	// fetched and expanded into denied domains by the caller at apply time.
	Sources []string

	// Refresh is how often Sources are re-fetched, from `refresh <dur>`.
	// Zero = none (fetch once).
	Refresh time.Duration

	// Line/Col record where the block appeared so cross-block conflicts
	// (duplicate policy names, dangling `use` references) can be reported
	// back to the right line by the caller.
	Line, Col int
}

// NamespaceDef is one `namespace <name> ( ... )` block: a named overlay of
// the per-namespace template subset (network / files / shares / env / agent).
// VM shape, includes and lifecycle hooks are sandbox-level concerns and are
// rejected inside a namespace body.
type NamespaceDef struct {
	Name     string    // required
	Template *Template // body parsed with the existing template grammar subset
}

// ParseFileString parses the typed-block grammar. Exactly one sandbox block
// is allowed per file (a second is an error at its line). Legacy flat files
// (any known top-level directive like `name`, `vm`, `network` outside a
// typed block) produce an error that includes a migration hint: wrap the
// body in `sandbox ( ... )` and move `name <x>` into the header.
func ParseFileString(src string) (*File, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	f := &File{}
	p.skipNewlines()
	for p.peek().Kind != TokEOF {
		t := p.peek()
		if t.Kind != TokIdent {
			return nil, p.errorAt(t, "expected block kind, got %s", t)
		}
		switch t.Val {
		case "clawk":
			if f.FormatVersion != 0 {
				return nil, p.errorAt(t, "duplicate 'clawk' version directive")
			}
			p.advance()
			v := p.peek()
			if v.Kind != TokIdent {
				return nil, p.errorAt(v, "expected format version after 'clawk', got %s", v)
			}
			n, err := strconv.Atoi(v.Val)
			if err != nil || n < 1 {
				return nil, p.errorAt(v, "invalid clawk format version %q: want a positive integer like 'clawk 1'", v.Val)
			}
			if n > FormatVersion {
				return nil, p.errorAt(v,
					"this file requires clawk.mod format version %d; this clawk understands up to %d — upgrade clawk",
					n, FormatVersion)
			}
			f.FormatVersion = n
			p.advance()
			if err := p.expectNewlineOrEOF(); err != nil {
				return nil, err
			}
		case "sandbox":
			if f.Sandbox != nil {
				return nil, p.errorAt(t,
					"second 'sandbox' block; exactly one sandbox block is allowed per file")
			}
			tmpl, err := p.parseSandboxBlock()
			if err != nil {
				return nil, err
			}
			f.Sandbox = tmpl
		case "policy":
			def, err := p.parsePolicyBlock()
			if err != nil {
				return nil, err
			}
			f.Policies = append(f.Policies, def)
		case "namespace":
			def, err := p.parseNamespaceBlock()
			if err != nil {
				return nil, err
			}
			f.Namespaces = append(f.Namespaces, def)
		case "includes":
			// A flat file opening with `includes` is a clawk.work — retired
			// with the typed-block grammar. Called out by name so the rename
			// hint lands instead of the generic wrap hint.
			return nil, p.errorAt(t,
				"clawk.work is retired — rename to clawk.mod and wrap the body in "+
					"`sandbox <name> ( includes ( ... ) ... )` — or run 'clawk mod migrate'")
		default:
			if isFlatDirective(t.Val) {
				return nil, p.errorAt(t,
					"top-level %q is retired: wrap the body in `sandbox ( ... )` "+
						"and move `name <x>` into the header — or run 'clawk mod migrate'", t.Val)
			}
			return nil, p.errorAt(t,
				"unknown block kind %q (want %s)", t.Val,
				describeFirst([]string{"sandbox", "policy", "namespace"}))
		}
		p.skipNewlines()
	}
	return f, nil
}

// isFlatDirective reports whether s is a top-level directive of the legacy
// flat grammar — the trigger for the wrap-in-sandbox migration hint.
// `includes` is deliberately absent: it gets the clawk.work-specific hint.
func isFlatDirective(s string) bool {
	switch s {
	case "name", "vm", "network", "forwards", "files", "shares",
		"skills", "agent", "env", "on":
		return true
	}
	return false
}

// parseSandboxBlock handles `sandbox [<name>] ( ... )`. The body is exactly
// the existing template grammar; the optional header name lands in
// Template.SandboxName. A `name` directive inside the body is rejected —
// the typed grammar carries the name in the header only.
func (p *parser) parseSandboxBlock() (*Template, error) {
	p.advance() // consume "sandbox"
	tmpl := &Template{}
	t := p.peek()
	if t.Kind == TokIdent {
		if isKeyword(t.Val) {
			return nil, p.errorAt(t, "sandbox name %q is a reserved keyword", t.Val)
		}
		tmpl.SandboxName = t.Val
		p.advance()
		t = p.peek()
	}
	if t.Kind != TokLParen {
		return nil, p.errorAt(t, "expected '(' after 'sandbox', got %s", t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			if err := p.expectNewlineOrEOF(); err != nil {
				return nil, err
			}
			return tmpl, nil
		}
		if t.Kind != TokIdent {
			return nil, p.errorAt(t, "expected directive or ')', got %s", t)
		}
		if t.Val == "name" {
			return nil, p.errorAt(t, "name moved to the block header: sandbox <name> ( ... )")
		}
		if err := p.parseTemplateDirective(tmpl, t); err != nil {
			return nil, err
		}
	}
}

// parsePolicyBlock handles `policy <name> ( ... )`. Body entries are one per
// line: `allow`/`deny` with the network-block entry grammar, `source "<url>"`
// for external blocklists, and `refresh <dur>` for their re-fetch cadence.
func (p *parser) parsePolicyBlock() (PolicyDef, error) {
	kw := p.peek()
	p.advance() // consume "policy"
	def := PolicyDef{Line: kw.Line, Col: kw.Col}
	t := p.peek()
	if t.Kind == TokLParen {
		return def, p.errorAt(t, "policy requires a name: policy <name> ( ... )")
	}
	if t.Kind != TokIdent {
		return def, p.errorAt(t, "expected policy name after 'policy', got %s", t)
	}
	if isKeyword(t.Val) {
		return def, p.errorAt(t, "policy name %q is a reserved keyword", t.Val)
	}
	def.Name = t.Val
	p.advance()
	t = p.peek()
	if t.Kind != TokLParen {
		return def, p.errorAt(t, "expected '(' after 'policy %s', got %s", def.Name, t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return def, p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return def, p.errorAt(t, "expected policy entry or ')', got %s", t)
		}
		switch t.Val {
		case "allow":
			p.advance()
			if err := p.parsePolicyEntry(&def, false); err != nil {
				return def, err
			}
		case "deny":
			p.advance()
			if err := p.parsePolicyEntry(&def, true); err != nil {
				return def, err
			}
		case "source":
			p.advance()
			u := p.peek()
			if u.Kind != TokString {
				return def, p.errorAt(u, "expected a quoted URL after 'source', got %s", u)
			}
			def.Sources = append(def.Sources, u.Val)
			p.advance()
		case "refresh":
			p.advance()
			v := p.peek()
			if v.Kind != TokIdent {
				return def, p.errorAt(v, "expected duration after 'refresh', got %s", v)
			}
			if def.Refresh != 0 {
				return def, p.errorAt(v, "duplicate 'refresh' directive")
			}
			d, err := time.ParseDuration(v.Val)
			if err != nil {
				return def, p.errorAt(v, "invalid refresh %q: want a duration like 24h", v.Val)
			}
			if d <= 0 {
				// Zero means "none" on PolicyDef; force the explicit spelling
				// (omit the directive) instead of accepting `refresh 0`.
				return def, p.errorAt(v, "refresh %q must be positive", v.Val)
			}
			def.Refresh = d
			p.advance()
		default:
			return def, p.errorAt(t,
				"unknown 'policy' directive %q (want %s)", t.Val,
				describeFirst([]string{"allow", "deny", "source", "refresh"}))
		}
	}
}

// parsePolicyEntry reads the value half of a policy allow/deny entry — the
// same grammar as a network-block entry (parseNetworkEntry), routed into
// PolicyDef's slices: bare domain, `ip <ADDR>`, or (deny only)
// `source "<url>"`, which joins the standalone `source` entries in Sources.
func (p *parser) parsePolicyEntry(def *PolicyDef, deny bool) error {
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
			def.DenyIPs = append(def.DenyIPs, ip.Val)
		} else {
			def.AllowIPs = append(def.AllowIPs, ip.Val)
		}
		p.advance()
		return nil
	}
	if deny && t.Val == "source" {
		p.advance()
		u := p.peek()
		if u.Kind != TokString {
			return p.errorAt(u, "expected a quoted URL after 'deny source', got %s", u)
		}
		def.Sources = append(def.Sources, u.Val)
		p.advance()
		return nil
	}
	if isKeyword(t.Val) {
		return p.errorAt(t, "unexpected keyword %q in policy entry", t.Val)
	}
	if deny {
		def.DenyDomains = append(def.DenyDomains, t.Val)
	} else {
		def.AllowDomains = append(def.AllowDomains, t.Val)
	}
	p.advance()
	return nil
}

// parseNamespaceBlock handles `namespace <name> ( ... )`. The body is the
// per-namespace subset of the template grammar: network, files, shares, env
// and agent. Sandbox-level directives (vm, includes, on) are rejected by
// name so the error explains the split instead of reading as a typo.
func (p *parser) parseNamespaceBlock() (NamespaceDef, error) {
	p.advance() // consume "namespace"
	def := NamespaceDef{Template: &Template{}}
	t := p.peek()
	if t.Kind == TokLParen {
		return def, p.errorAt(t, "namespace requires a name: namespace <name> ( ... )")
	}
	if t.Kind != TokIdent {
		return def, p.errorAt(t, "expected namespace name after 'namespace', got %s", t)
	}
	if isKeyword(t.Val) {
		return def, p.errorAt(t, "namespace name %q is a reserved keyword", t.Val)
	}
	def.Name = t.Val
	p.advance()
	t = p.peek()
	if t.Kind != TokLParen {
		return def, p.errorAt(t, "expected '(' after 'namespace %s', got %s", def.Name, t)
	}
	p.advance()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Kind == TokRParen {
			p.advance()
			return def, p.expectNewlineOrEOF()
		}
		if t.Kind != TokIdent {
			return def, p.errorAt(t, "expected namespace directive or ')', got %s", t)
		}
		var err error
		switch t.Val {
		case "network":
			err = p.parseNetworkBlock(def.Template)
		case "files":
			err = p.parseFilesBlock(def.Template)
		case "shares":
			err = p.parseSharesBlock(def.Template)
		case "env":
			err = p.parseEnvBlock(&def.Template.Env)
		case "agent":
			err = p.parseAgentBlock(def.Template)
		case "vm", "includes", "on":
			err = p.errorAt(t,
				"%q is a sandbox-level directive, not allowed in a namespace (want %s)",
				t.Val, describeFirst([]string{"network", "files", "shares", "env", "agent"}))
		default:
			err = p.errorAt(t,
				"unknown 'namespace' directive %q (want %s)", t.Val,
				describeFirst([]string{"network", "files", "shares", "env", "agent"}))
		}
		if err != nil {
			return def, err
		}
	}
}

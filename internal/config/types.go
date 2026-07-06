package config

import (
	"fmt"
	"slices"
	"strconv"
	"time"
)

type PhaseStatus string

const (
	PhaseStatusPending PhaseStatus = "pending"
	PhaseStatusActive  PhaseStatus = "active"
	PhaseStatusMerged  PhaseStatus = "merged"
)

type Phase struct {
	Repo     string      `json:"repo"`
	Branch   string      `json:"branch"`
	Status   PhaseStatus `json:"status"`
	Order    int         `json:"order"`
	Worktree string      `json:"worktree,omitempty"` // path to the git worktree on host
	Setup    []string    `json:"setup,omitempty"`    // commands to run in the VM after every boot (template `on up`)

	// OnCreate is the list of commands to run once after the very first
	// boot of the sandbox, before the runner attaches. Sourced from the
	// repo Clawkfile's `on create` block. Hard-fails the up — see
	// Sandbox.CreatePending.
	OnCreate []string `json:"on_create,omitempty"`
	// OnCreateAt records the wall-clock time `on create` last completed
	// successfully for this phase. Zero means it has not run yet (or the
	// sandbox is in the create-pending state). Used by up.go to decide
	// whether to run `on create` again after a failure.
	OnCreateAt time.Time `json:"on_create_at,omitempty"`

	// InPlace, if true, means Worktree points at the user's actual
	// directory rather than a dedicated git worktree. Set by
	// `clawk here`. Destroy skips `git worktree remove` for these.
	InPlace bool `json:"in_place,omitempty"`
}

type VMState string

// VMState values are persisted in sandbox records: frozen — never rename
// an existing value, only add. Same rule for StopReason, Provider and the
// BlockOrigin* constants below; the vfkit→vz Provider rename left
// Normalize() carrying migration code forever, which is the tax this rule
// avoids.
const (
	VMStateStopped VMState = "stopped"
	VMStateRunning VMState = "running"
)

// StopReason qualifies a VMStateStopped that the user didn't ask for.
// Values are persisted in sandbox records — frozen (see VMState).
type StopReason string

// StopReasonIdle marks a VM the daemon stopped after the sandbox sat idle
// past its idle timeout. Distinct from an explicit `clawk down` so the
// CLI can render "stopped (idle)" and the attach path knows a mid-attach
// shutdown was a park it should transparently boot through.
const StopReasonIdle StopReason = "idle"

// StopReasonSuspended marks a VM stopped by `clawk snapshot`: its memory +
// device state sits in a suspend file next to the VM, and the next boot
// (resume, up, or any attach verb) restores it exactly where it left off
// instead of cold-booting. Rendered as "stopped (suspended)".
const StopReasonSuspended StopReason = "suspended"

// NetworkPolicy controls outbound network access from the sandbox.
//
// AllowedDomains support wildcards like "*.example.com" and are matched
// at DNS-resolution time. AllowedIPs accept plain addresses or CIDR
// ranges like "10.0.0.0/24" and are checked on every TCP SYN, catching
// direct-IP connections (useful for provisioning new servers that don't
// have DNS yet).
type NetworkPolicy struct {
	// AllowedDomains, AllowedIPs and DeniedDomains are the legacy flat
	// policy of pre-block sandbox records. Normalize folds them into the
	// "custom" block on load; new records are written block-shaped and
	// leave these empty.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	AllowedIPs     []string `json:"allowed_ips,omitempty"`
	// DeniedDomains are blocked outright — the domain and every subdomain.
	// A block overrides any allow and suppresses the interactive prompt, so
	// the agent is refused immediately without asking again. Entries are
	// registrable (root) domains: "telemetry.example.com" is blocked by an
	// entry of "example.com".
	DeniedDomains []string `json:"denied_domains,omitempty"`

	// Use names the policies whose blocks form the base of this sandbox's
	// chain, in increasing precedence. nil means the chain was never made
	// explicit and resolves to ["default"]; a non-nil list is complete —
	// include "default" where wanted.
	Use []string `json:"use,omitempty"`
	// Blocks are the sandbox's own policy layers, lowest precedence first.
	// They sit above every Use-referenced policy; the "custom" block (CLI
	// edits and persisted interactive grants) stays last, above the "mod"
	// block (entries composed from clawk.mod).
	Blocks []NetworkBlock `json:"blocks,omitempty"`
}

// NetworkBlock is one origin-labeled layer of a sandbox's network policy.
// Origins mirror the chain in the design doc: "namespace" and "mod" carry
// file-composed entries, "custom" carries CLI edits and persisted
// interactive grants. Policy-referenced layers ("default", "policy",
// "source") are never stored on the sandbox — they resolve from Use at
// up/reload so a policy edit propagates without rewriting sandbox records.
type NetworkBlock struct {
	Origin       string   `json:"origin"`
	Name         string   `json:"name,omitempty"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	AllowIPs     []string `json:"allow_ips,omitempty"`
	DenyDomains  []string `json:"deny_domains,omitempty"`
	DenyIPs      []string `json:"deny_ips,omitempty"`
}

// Block origins stored on sandbox records, lowest to highest precedence.
// Frozen (see VMState) — and doubly so here: blockOriginRank sends
// unknown strings to the lowest precedence, so a renamed origin would
// silently invert which rules win.
const (
	BlockOriginNamespace = "namespace"
	BlockOriginMod       = "mod"
	BlockOriginCustom    = "custom"
)

// blockOriginRank orders stored blocks; unknown origins sort first so a
// record written by a newer clawk never outranks the user's custom block.
func blockOriginRank(origin string) int {
	switch origin {
	case BlockOriginNamespace:
		return 1
	case BlockOriginMod:
		return 2
	case BlockOriginCustom:
		return 3
	default:
		return 0
	}
}

// Normalize migrates a legacy flat record into block form and restores the
// block-order invariant. Idempotent; the store applies it on every load.
func (n *NetworkPolicy) Normalize() {
	if len(n.AllowedDomains)+len(n.AllowedIPs)+len(n.DeniedDomains) > 0 {
		custom := n.Block(BlockOriginCustom)
		custom.AllowDomains = append(custom.AllowDomains, n.AllowedDomains...)
		custom.AllowIPs = append(custom.AllowIPs, n.AllowedIPs...)
		custom.DenyDomains = append(custom.DenyDomains, n.DeniedDomains...)
		n.AllowedDomains, n.AllowedIPs, n.DeniedDomains = nil, nil, nil
	}
	slices.SortStableFunc(n.Blocks, func(a, b NetworkBlock) int {
		return blockOriginRank(a.Origin) - blockOriginRank(b.Origin)
	})
}

// Block returns the policy's layer with the given origin, appending an empty
// one if absent. The returned pointer is valid until Blocks next grows —
// mutate it before any further Block call.
func (n *NetworkPolicy) Block(origin string) *NetworkBlock {
	for i := range n.Blocks {
		if n.Blocks[i].Origin == origin {
			return &n.Blocks[i]
		}
	}
	n.Blocks = append(n.Blocks, NetworkBlock{Origin: origin})
	return &n.Blocks[len(n.Blocks)-1]
}

// Provider identifies which VM backend runs a sandbox. Values are
// persisted in sandbox records and clawk.mod files — frozen (see
// VMState); legacyProviderVFKit below is what a rename costs.
type Provider string

const (
	// ProviderVZ runs sandboxes on macOS via Apple's
	// Virtualization.framework (no external VMM binary) with gvproxy for
	// userspace networking and ACL enforcement. The default on macOS.
	ProviderVZ Provider = "vz"
	// ProviderFirecracker runs sandboxes on Linux via Firecracker.
	// Networking is TAP-on-bridge with no host-side filtering.
	ProviderFirecracker Provider = "firecracker"

	// legacyProviderVFKit is the retired identifier for ProviderVZ. The vz
	// provider (in-process Apple Virtualization.framework) was originally
	// called "vfkit", after the binary it used to drive as a subprocess.
	// Kept only so sandboxes and clawk.mod files written under the old name
	// still resolve — see Normalize.
	legacyProviderVFKit Provider = "vfkit"
)

// Normalize maps retired provider identifiers onto their current equivalent
// so configs written before a rename keep working. Unknown values pass
// through unchanged for the caller to reject with a clear error.
func (p Provider) Normalize() Provider {
	if p == legacyProviderVFKit {
		return ProviderVZ
	}
	return p
}

// DefaultAllowedDomains are domains allowed by default for development.
// Originally seeded from Andrew Lock's microVM sandbox allow list:
// https://andrewlock.net/running-ai-agents-safely-in-a-microvm-using-docker-sandbox/
//
// Every entry here is a contract in both directions: removing one after
// release breaks users who rely on it, and each one is a destination the
// agent can reach without asking — i.e. a potential exfiltration endpoint.
// Additions must be (a) operated by the organization they claim to serve
// and (b) needed by a mainstream development workflow, not one project.
// When in doubt, leave it out — users can `clawk network allow` per
// sandbox or per namespace.
var DefaultAllowedDomains = []string{
	// --- AI services ---
	"*.chatgpt.com",
	"*.oaistatic.com",
	"*.oaiusercontent.com",
	"*.openai.com",
	"*.claude.ai",
	"api.anthropic.com",
	"api.perplexity.ai",
	"claude.ai",
	"gemini.google.com",
	"generativelanguage.googleapis.com",
	"models.dev", // model catalog fetched by the opencode runner
	"platform.claude.com",
	"play.googleapis.com",
	"statsig.anthropic.com",

	// --- Package managers ---
	"*.bun.sh",
	"*.gradle.org",
	"*.packagist.org",
	"*.yarnpkg.com",
	"apache.org",
	"astral.sh",
	"bootstrap.pypa.io",
	"cocoapods.org",
	"cpan.org",
	"crates.io",
	"dot.net",
	"dotnet.microsoft.com",
	"eclipse.org",
	"files.pythonhosted.org",
	"go.dev",
	"golang.org",
	"goproxy.io",
	"haskell.org",
	"hex.pm",
	"index.crates.io",
	"java.com",
	"java.net",
	"maven.org",
	"metacpan.org",
	"nodejs.org",
	"nodesource.com",
	"npm.duckdb.org",
	"npmjs.com",
	"npmjs.org",
	"nuget.org",
	"packagist.com",
	"pkg.go.dev",
	"proxy.golang.org",
	"pub.dev",
	"pypa.io",
	"pypi.org",
	"pypi.python.org",
	"pythonhosted.org",
	"registry.npmjs.org",
	"repo.maven.apache.org",
	"ruby-lang.org",
	"rubygems.org",
	"rubyonrails.org",
	"rustup.rs",
	"rvm.io",
	"sh.rustup.rs",
	"spring.io",
	"static.crates.io",
	"static.rust-lang.org",
	"sum.golang.org",
	"swift.org",
	"tuf-repo-cdn.sigstore.dev",
	"ziglang.org",

	// --- Code hosts and container registries ---
	"*.business.githubcopilot.com",
	"*.docker.com",
	"*.docker.io",
	"*.gcr.io",
	"*.github.com",
	"*.githubusercontent.com",
	"*.gitlab.com",
	"*.production.cloudflare.docker.com",
	"bitbucket.org",
	"dhi.io",
	"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
	"ghcr.io",
	"github.com",
	"k8s.io",
	"mcr.microsoft.com",
	"ppa.launchpad.net",
	"public.ecr.aws",
	"quay.io",
	"registry.k8s.io",
	"sourceforge.net",

	// --- Cloud infrastructure ---
	"*.amazonaws.com",
	"*.googleapis.com",
	"*.googleusercontent.com",
	"*.gstatic.com",
	"*.gvt1.com",
	"*.public.blob.vercel-storage.com",
	"*.visualstudio.com",
	"apis.google.com",
	"app.daytona.io",
	"azure.com",
	"binaries.prisma.sh",
	"challenges.cloudflare.com",
	"clerk.com",
	"csp.withgoogle.com",
	"dev.azure.com",
	"dl.google.com",
	"fastly.com",
	"figma.com",
	"hashicorp.com",
	"jsdelivr.net",
	"json-schema.org",
	"json.schemastore.org",
	"login.microsoftonline.com",
	"mise-versions.jdx.dev",
	"mise.run",
	"packages.microsoft.com",
	"play.google.com",
	"playwright.azureedge.net", // legacy CDN, still used for some older versions
	"*.playwright.dev",         // current CDN (cdn.playwright.dev for Chromium/Firefox/WebKit)
	"*.prss.microsoft.com",     // redirect target of cdn.playwright.dev/dbazure/... (MS downloads)
	"supabase.com",
	"unpkg.com",
	"vercel.com",
	"www.google.com",

	// --- OS packages ---
	"*.debian.org",
	"alpinelinux.org",
	"apt.llvm.org",
	"archive.ubuntu.com",
	"archlinux.org",
	"centos.org",
	"debian.org",
	"dl-cdn.alpinelinux.org",
	"fedoraproject.org",
	"packagecloud.io",
	"ports.ubuntu.com",
	"security.ubuntu.com",
	"ubuntu.com",
	"*.launchpad.net",
	"*.snapcraft.io",         // metadata + API (api.snapcraft.io, dashboard.snapcraft.io, ...)
	"*.snapcraftcontent.com", // the CDN snap redirects to for actual .snap downloads
}

// PortForward maps a host port to a guest port so services running in the VM
// are reachable from the host (e.g., dev servers). Applied at VM start time;
// changes require an `up` cycle to take effect.
type PortForward struct {
	HostPort  int `json:"host_port"`
	GuestPort int `json:"guest_port"`
}

func (p PortForward) String() string {
	if p.HostPort == p.GuestPort {
		return strconv.Itoa(p.HostPort)
	}
	return fmt.Sprintf("%d:%d", p.HostPort, p.GuestPort)
}

// HostFile is a snapshot-on-up file pushed from host into guest. Sourced
// from clawk.mod `files (...)` and refreshed on every `clawk up` — edits
// on the host are NOT live (use HostShare for that). HostPath is tilde-
// and env-expanded at compose time; Mode == 0 preserves the host file's
// permissions.
type HostFile struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	Mode      uint32 `json:"mode,omitempty"`
}

// HostShare is a virtio-fs live mount from host directory into guest.
// Sourced from clawk.mod `shares (...)`. Reflects on the host
// immediately — credentials that rotate underneath (~/.aws after `aws sts
// assume-role`, ~/.config/gcloud) stay in sync without a clawk-up cycle.
// Defaults to ReadOnly=true at parse time so an accidental in-VM write
// can't clobber the host credential.
type HostShare struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only"`
}

// DefaultNamespace is the grouping a sandbox belongs to when none is set.
const DefaultNamespace = "default"

type Sandbox struct {
	Name     string   `json:"name"`
	Provider Provider `json:"provider"`
	Profile  string   `json:"profile,omitempty"` // active overlay profile (if any)
	// Namespace groups the sandbox for organization and (Phase 2)
	// per-namespace defaults. Empty is treated as DefaultNamespace; use
	// NamespaceName for the resolved value.
	Namespace string `json:"namespace,omitempty"`
	// Anchor is the directory this sandbox is bound to, set for sandboxes
	// created by the bare `clawk` invocation (addressed by being in the
	// directory rather than by a typed name). Empty means the sandbox is
	// explicitly named. Its presence is the cwd-vs-ticket discriminator.
	Anchor string `json:"anchor,omitempty"`
	// DesiredState is the lifecycle state the user wants: VMStateRunning after
	// `clawk up`, VMStateStopped after `clawk down`. Empty means no explicit
	// intent yet. It's *spec* (desired), distinct from VMState below (*status*,
	// observed) — and it's what a future server-side reconciler converges to,
	// so the imperative up/down commands are already declarative edits.
	DesiredState VMState `json:"desired_state,omitempty"`
	// ResourceVersion increments on every spec write. Groundwork for optimistic
	// concurrency once there are multiple writers (the cloud control plane);
	// today it's just a monotonic "this record changed" counter.
	ResourceVersion int `json:"resource_version,omitempty"`
	// RecordSchema is the schema version of this record's JSON shape,
	// stamped by Store.Save on every write (RecordSchemaVersion). Distinct
	// from the store-wide version in meta.json: that one drives directory
	// migrations, this one says which clawk shape wrote THIS record, so a
	// selective per-record migration is possible without guesswork. Zero
	// means the record predates the field: schema 1.
	RecordSchema int           `json:"record_schema,omitempty"`
	Phases       []Phase       `json:"phases"`
	Network      NetworkPolicy `json:"network"`
	Forwards     []PortForward `json:"forwards,omitempty"`
	// Files is the list of host->guest file copies refreshed on every
	// `clawk up`. See HostFile. Empty = no snapshots.
	Files []HostFile `json:"files,omitempty"`
	// Shares is the list of host->guest virtio-fs live mounts. See
	// HostShare. Empty = no live mounts.
	//
	// Changes to this list require `clawk down && clawk up` to re-emit
	// the provider's device list, but the disk image survives.
	Shares []HostShare `json:"shares,omitempty"`
	// Instructions are extra persistent-guidance blocks rendered into the
	// generated workspace CLAUDE.md (see sandbox.WorkspaceDocFile), sourced
	// from the namespace and a repo's clawk.mod. Each entry is a markdown
	// block read on every boot — the place for "always ask before X" or
	// project conventions that must survive a throwaway VM.
	Instructions []string `json:"instructions,omitempty"`
	// Memory is seed content for the agent's auto-memory MEMORY.md, sourced
	// from the namespace and clawk.mod. Written into the memory dir once on
	// first boot and never afterward (see sandbox.SeedClaudeMemory), so a
	// fresh sandbox starts with baseline knowledge without clobbering memory
	// the agent has since accumulated.
	Memory string `json:"memory,omitempty"`
	// Image is the OCI image reference this sandbox boots as its root
	// filesystem (clawk.mod `vm ( image <ref> )`). The provider builds an
	// ext4 rootfs from the image (with clawk-init and the pty-agent
	// injected), boots it via direct-kernel, and the sandbox runs
	// sshd-free — all host access goes over the vsock agent.
	Image string `json:"image,omitempty"`
	// Kernel overrides the guest kernel the vz provider direct-boots: a
	// local vmlinux path or an http(s) URL. Empty = the default Kata
	// kernel. Declared via clawk.mod `vm ( kernel <path|url> )` or the
	// --kernel flag. The main use is supplying a KVM-enabled kernel for
	// nested virtualization (the stock Kata kernel has KVM disabled, so
	// the guest has no /dev/kvm).
	Kernel string `json:"kernel,omitempty"`
	// GuestABI records the guest-contract version (the clawk-init boot
	// manifest schema + the pty-agent vsock protocol; see
	// sandbox.CurrentGuestABI) baked into this sandbox's disk at create.
	// Guest binaries are never updated in place, so this is the record a
	// later host consults to fail readably ("recreate this sandbox")
	// instead of hitting an in-guest version error mid-boot. Zero means
	// the record predates the field: ABI 1.
	GuestABI int `json:"guest_abi,omitempty"`
	// RequiredEnv holds the names (not values) of host env vars this
	// sandbox wants exported inside the VM. Declared in clawk.mod via
	// `env ( NAME ... )`. Values are read from the host shell at
	// sandbox-create time and written to /etc/profile.d/99-clawk-env.sh
	// in the guest — never persisted to disk on the host alongside the
	// name so we don't check secrets into the sandbox state file.
	RequiredEnv []string `json:"required_env,omitempty"`
	// NestedVirt opts the VM into hardware-assisted nested
	// virtualization so the guest can run its own VMs (Docker with KVM,
	// Firecracker, etc.). Requires macOS 15+ and M3-or-newer Apple
	// Silicon. Enabled per-sandbox at create time; changes require a
	// destroy+recreate to take effect.
	NestedVirt bool `json:"nested_virt,omitempty"`

	// CPU is the vCPU count exposed to the guest. Zero means the provider
	// picks a default. Not a reservation — KVM and VZ don't charge host CPU
	// time for idle vCPUs.
	CPU uint `json:"cpu,omitempty"`

	// MemoryMiB is the baseline memory target in mebibytes. When
	// MemoryMaxMiB > MemoryMiB, providers that support virtio-balloon
	// reclaim (max - baseline) back to the host at boot and let the guest
	// grow on demand via deflate_on_oom. Zero = no ballooning.
	MemoryMiB uint64 `json:"memory_mib,omitempty"`

	// MemoryMaxMiB is the guest-visible hard cap on memory in mebibytes —
	// the amount allocated at boot. Zero = provider default.
	MemoryMaxMiB uint64 `json:"memory_max_mib,omitempty"`

	// IdleTimeoutSec is how long the sandbox may sit idle (no attached
	// session, quiescent guest) before its VM daemon stops it to reclaim
	// host memory. Zero = the built-in default; negative = never stop.
	// Declared via clawk.mod `vm ( idle_timeout <dur|off> )`. An idle stop
	// is a park, not a `clawk down`: DesiredState stays running and any
	// attach boots the VM back.
	IdleTimeoutSec int64 `json:"idle_timeout_sec,omitempty"`

	// --- Status: observed runtime state, NOT user-authoritative ---------------
	// A reconciled cache; the provider/OS is the source of truth. Reconcile via
	// observe() before trusting these for a decision (clawk list/status/migrate
	// already do).
	VMState VMState `json:"vm_state"`
	// StopReason records why the VM last left the running state, when the
	// stop wasn't an explicit user verb. Today the only value is
	// StopReasonIdle (the daemon parked an idle VM). Cleared on every boot
	// and on explicit `clawk down`, so a bare "stopped" always means the
	// user asked for it.
	StopReason StopReason `json:"stop_reason,omitempty"`
	VMPid      int        `json:"vm_pid,omitempty"`
	GuestIP    string     `json:"guest_ip,omitempty"`
	GatewayIP  string     `json:"gateway_ip,omitempty"`
	MACAddress string     `json:"mac_address,omitempty"`

	// CreatePending is set when one of the phase `on create` commands has
	// failed at least once. The VM is left running so the user can shell in
	// and investigate; runner attach is refused with an actionable message;
	// the next `clawk up` re-runs `on create` from scratch. `clawk destroy`
	// is the explicit reset.
	CreatePending bool `json:"create_pending,omitempty"`
	// CreatePendingReason carries the human-readable failure (phase name +
	// failing command + provider error) so `clawk status` can surface it
	// without re-running anything. Cleared on a successful `on create`.
	CreatePendingReason string `json:"create_pending_reason,omitempty"`

	// SessionProject is the stable identifier of the session-history repo
	// this sandbox's Claude Code conversations belong to (see
	// internal/sessions.ProjectID). Sandboxes that work on the same repo set
	// share one history repo — each on its own branch — so a fresh sandbox
	// for the same project boots with prior transcripts and memory. Computed
	// and persisted on first bring-up; empty on sandboxes created before the
	// feature, in which case it is derived again from the phases. Currently
	// populated by the vz provider only.
	SessionProject string `json:"session_project,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// PRRefreshedAt is the wall-clock time of the last successful
	// PR-state refresh via `gh`. v2 derives Phase.Status from PR
	// state; this timestamp gates a 60-second cache so `clawk
	// status` doesn't shell out on every invocation.
	PRRefreshedAt time.Time `json:"pr_refreshed_at,omitempty"`
}

// DisplayName is the human-facing form of a store key. It currently returns
// the key unchanged — anchored sandboxes are keyed by a clean `<base>` and
// carry their binding in Sandbox.Anchor, so no key needs rewriting for display.
// Retained as the single seam every surface (`clawk list`/`status`, the
// workspace CLAUDE.md, the menubar) routes through, should display names ever
// need to diverge from keys again.
func DisplayName(key string) string {
	return key
}

// DisplayName returns the sandbox's human-facing name. See DisplayName.
func (s *Sandbox) DisplayName() string {
	return DisplayName(s.Name)
}

// NamespaceName is the sandbox's namespace, resolving the empty zero value to
// DefaultNamespace so callers never have to special-case it.
func (s *Sandbox) NamespaceName() string {
	if s.Namespace == "" {
		return DefaultNamespace
	}
	return s.Namespace
}

// Key is the sandbox's store key, "<namespace>/<name>" — the identity the
// store resolves to an on-disk location. Bare names passed to the store (e.g.
// from CLI args) resolve to the default namespace; this is the explicit form
// for callers that hold the sandbox.
func (s *Sandbox) Key() string {
	return s.NamespaceName() + "/" + s.Name
}

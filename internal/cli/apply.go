package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/spf13/cobra"
)

var (
	applyFile   string
	applyDryRun bool
)

func init() {
	applyCmd.Flags().StringVarP(&applyFile, "file", "f", "", "manifest file or directory to apply (clawk.mod syntax)")
	applyCmd.Flags().BoolVar(&applyDryRun, "dry-run", false, "parse and report without writing")
	_ = applyCmd.MarkFlagRequired("file")
	rootCmd.AddCommand(applyCmd)
}

// applyCmd is hidden for the initial release: it drives the namespace and
// policy surface (a Kubernetes-flavored power feature) which is kept working
// in code but held back from the launch-facing help.
var applyCmd = &cobra.Command{
	Use:    "apply -f <file-or-dir>",
	Hidden: true,
	Short:  "Apply resource manifests (policies and namespaces, clawk.mod syntax)",
	Long: `apply reads one manifest file — or every regular file in a directory —
written in the clawk.mod typed-block grammar and upserts each resource it
declares. It's idempotent: keep the manifests in git and re-apply to
reproduce the same policies and namespaces anywhere.

  policy corp-egress (
      allow github.com
      allow ip 10.20.0.0/16
      deny  telemetry.corp.com
  )

  namespace work (
      network (
          use  default corp-egress
          allow *.githubusercontent.com
          deny  tracker.example.com
          deny  source "https://big.oisd.nl/domainswild"
      )
      files ( ~/work-context.md /home/agent/workspace/WORK_CONTEXT.md )
      env   ( CORP_TOKEN )
  )

'deny source "<url>"' registers the external blocklist (hosts/EasyList/uBlock)
as a source policy appended to the namespace's 'use' chain — refreshed via
'clawk policy refresh', never baked into the namespace record. Sandbox blocks
are not accepted here; a sandbox template lives in its repo's clawk.mod.

With a directory, files apply independently: one file's error doesn't stop
the others, and the exit status is nonzero if any failed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		info, err := os.Stat(applyFile)
		if err != nil {
			return fmt.Errorf("reading manifest: %w", err)
		}
		if !info.IsDir() {
			return applyManifestFile(out, applyFile)
		}
		entries, err := os.ReadDir(applyFile)
		if err != nil {
			return fmt.Errorf("reading manifest dir: %w", err)
		}
		var applied int
		var failed []string
		for _, e := range entries {
			if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			path := filepath.Join(applyFile, e.Name())
			if err := applyManifestFile(out, path); err != nil {
				// Per-file failures are reported with the filename and the
				// remaining files still apply; the summary error below makes
				// the exit status nonzero.
				fmt.Fprintf(cmd.ErrOrStderr(), "error: %v\n", err)
				failed = append(failed, e.Name())
				continue
			}
			applied++
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d manifest(s) failed: %s",
				len(failed), applied+len(failed), strings.Join(failed, ", "))
		}
		if applied == 0 {
			return fmt.Errorf("no manifest files in %s", applyFile)
		}
		return nil
	},
}

// applyManifestFile parses and applies one manifest. Every error is prefixed
// with the path so the directory loop's per-file reports stay attributable.
func applyManifestFile(out io.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	f, err := template.ParseFileString(string(data))
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if f.Sandbox != nil {
		return fmt.Errorf("%s: registering sandbox templates via apply is not supported yet", path)
	}
	if len(f.Policies) == 0 && len(f.Namespaces) == 0 {
		return fmt.Errorf("%s: no policy or namespace blocks to apply", path)
	}

	verb := "applied"
	if applyDryRun {
		verb = "would apply"
	}
	for _, def := range f.Policies {
		p, err := policyFromDef(def)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !applyDryRun {
			if err := store.SavePolicy(p); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		fmt.Fprintf(out, "%s policy %q: %d allow, %d deny, source %s\n",
			verb, p.Name,
			len(p.AllowDomains)+len(p.AllowIPs), len(p.DenyDomains)+len(p.DenyIPs),
			orDash(p.Source))
	}
	for _, def := range f.Namespaces {
		ns, err := namespaceFromDef(def, applyDryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !applyDryRun {
			if err := store.SaveNamespace(ns); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		fmt.Fprintf(out, "%s namespace %q: %d allow, %d deny, %d use, %d files, %d env\n",
			verb, ns.Name,
			len(ns.AllowedDomains)+len(ns.AllowedIPs), len(ns.DeniedDomains)+len(ns.DeniedIPs),
			len(ns.Use), len(ns.Files), len(ns.Env))
	}
	return nil
}

// orDash renders an optional field for the apply report.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// namespaceFromDef maps a `namespace <name> ( ... )` block to a Namespace
// config: network allow/deny (a bare `deny *` is the default-deny baseline
// and dropped — clawk is already default-deny), path-resolved files/shares,
// env, and the `use` chain. External blocklists (`deny source "<url>"`)
// register as source policies appended to the chain rather than being
// fetched and baked in, so a refresh reaches every member sandbox.
func namespaceFromDef(def template.NamespaceDef, dryRun bool) (*config.Namespace, error) {
	if err := validateNamespaceName(def.Name); err != nil {
		return nil, err
	}
	tmpl := def.Template

	var fileSources []fileSource
	for _, f := range tmpl.Files {
		fileSources = append(fileSources, fileSource{Origin: "manifest", Spec: f})
	}
	files, err := composeFiles(fileSources)
	if err != nil {
		return nil, err
	}
	var shareSources []shareSource
	for _, s := range tmpl.Shares {
		shareSources = append(shareSources, shareSource{Origin: "manifest", Spec: s})
	}
	shares, err := composeShares(shareSources)
	if err != nil {
		return nil, err
	}

	denied := make([]string, 0, len(tmpl.DenyDomains))
	for _, d := range tmpl.DenyDomains {
		if d == "*" {
			continue // explicit default-deny baseline; clawk is already default-deny
		}
		denied = append(denied, d)
	}

	use, err := spliceSourcePolicies(tmpl.Use, tmpl.Use != nil, tmpl.DenySources, dryRun)
	if err != nil {
		return nil, err
	}

	return &config.Namespace{
		Name:           def.Name,
		AllowedDomains: dedupStrings(tmpl.Domains),
		AllowedIPs:     dedupStrings(tmpl.IPs),
		DeniedDomains:  dedupStrings(denied),
		DeniedIPs:      dedupStrings(tmpl.DenyIPs),
		Use:            use,
		Files:          files,
		Shares:         shares,
		Env:            dedupStrings(tmpl.Env),
	}, nil
}

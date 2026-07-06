package template

import (
	"errors"
	"strings"
	"testing"
)

func TestMigrateFlatMovesNameIntoHeader(t *testing.T) {
	src := `// project config
name my-project

vm (
    cpu 4
)

network (
    allow api.example.com   // keep
    deny  ip 10.9.9.9
)
`
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	if !strings.HasPrefix(out, "sandbox my-project (\n") {
		t.Fatalf("header missing name:\n%s", out)
	}
	for _, want := range []string{"// project config", "allow api.example.com   // keep", "deny  ip 10.9.9.9"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatting not preserved, missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name my-project") {
		t.Errorf("name directive should be gone from the body:\n%s", out)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse: %v", err)
	}
	if f.Sandbox.SandboxName != "my-project" {
		t.Errorf("Sandbox.SandboxName = %q, want my-project", f.Sandbox.SandboxName)
	}
	if f.Sandbox.CPU != 4 || len(f.Sandbox.Domains) != 1 || len(f.Sandbox.DenyIPs) != 1 {
		t.Errorf("migrated content lost: %+v", f.Sandbox)
	}
}

func TestMigrateFlatWithoutName(t *testing.T) {
	out, err := MigrateFlat("network (\n    allow x.dev\n)\n")
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	if !strings.HasPrefix(out, "sandbox (\n") {
		t.Fatalf("expected bare sandbox header:\n%s", out)
	}
	if _, err := ParseFileString(out); err != nil {
		t.Fatalf("migrated output does not parse: %v", err)
	}
}

func TestMigrateFlatWorkspaceBody(t *testing.T) {
	src := "name acme\n\nincludes (\n    ./api\n    ./web\n)\n\nnetwork (\n    allow github.com\n)\n"
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse: %v", err)
	}
	if f.Sandbox.SandboxName != "acme" || len(f.Sandbox.Includes) != 2 {
		t.Errorf("workspace shape lost: %+v", f.Sandbox)
	}
}

func TestMigrateFlatHoistsTopLevelVMScalars(t *testing.T) {
	// The earliest releases accepted vm scalars at top level, before the
	// vm ( … ) grouping existed; such files are still in the wild.
	src := "name legacy\nnested\nprovider vz\n\nnetwork (\n    allow x.dev\n)\n"
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse:\n%s\nerr: %v", out, err)
	}
	if !f.Sandbox.Nested || f.Sandbox.Provider != "vz" {
		t.Errorf("hoisted scalars lost: nested=%v provider=%q\n%s",
			f.Sandbox.Nested, f.Sandbox.Provider, out)
	}
}

func TestMigrateFlatHoistsScalarsIntoExistingVMBlock(t *testing.T) {
	src := "name legacy\nnested\n\nvm (\n    cpu 4\n)\n"
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse:\n%s\nerr: %v", out, err)
	}
	if !f.Sandbox.Nested || f.Sandbox.CPU != 4 {
		t.Errorf("scalar merge lost values: nested=%v cpu=%d\n%s",
			f.Sandbox.Nested, f.Sandbox.CPU, out)
	}
	if strings.Count(out, "vm (") != 1 {
		t.Errorf("expected the scalars inside the existing vm block:\n%s", out)
	}
}

func TestMigrateFlatHoistsTopLevelNetworkEntries(t *testing.T) {
	// Pre-grouping files also carried bare allow/deny at top level.
	src := "name clawkwork\nnested\nallow *.clawk.work\n"
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse:\n%s\nerr: %v", out, err)
	}
	if f.Sandbox.SandboxName != "clawkwork" || !f.Sandbox.Nested {
		t.Errorf("header/vm scalars lost:\n%s", out)
	}
	if len(f.Sandbox.Domains) != 1 || f.Sandbox.Domains[0] != "*.clawk.work" {
		t.Errorf("hoisted allow lost: %v\n%s", f.Sandbox.Domains, out)
	}
}

func TestMigrateFlatHoistsEntriesIntoExistingNetworkBlock(t *testing.T) {
	src := "allow extra.dev\n\nnetwork (\n    allow x.dev\n    deny ip 10.0.0.9\n)\n"
	out, err := MigrateFlat(src)
	if err != nil {
		t.Fatalf("MigrateFlat: %v", err)
	}
	f, err := ParseFileString(out)
	if err != nil {
		t.Fatalf("migrated output does not parse:\n%s\nerr: %v", out, err)
	}
	if len(f.Sandbox.Domains) != 2 || len(f.Sandbox.DenyIPs) != 1 {
		t.Errorf("network merge lost entries: %+v\n%s", f.Sandbox, out)
	}
	if strings.Count(out, "network (") != 1 {
		t.Errorf("expected entries inside the existing network block:\n%s", out)
	}
}

func TestMigrateFlatAlreadyTyped(t *testing.T) {
	_, err := MigrateFlat("sandbox demo (\n    network ( allow x.dev )\n)\n")
	if !errors.Is(err, ErrAlreadyTyped) {
		t.Fatalf("err = %v, want ErrAlreadyTyped", err)
	}
}

func TestMigrateFlatGarbageErrors(t *testing.T) {
	if _, err := MigrateFlat("this is ( not a clawk.mod"); err == nil {
		t.Fatal("expected an error for unmigratable input")
	}
}

package pluginpkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestV2RequiresProvides(t *testing.T) {
	root := t.TempDir()
	writeV2Plugin(t, root, `{
  "apiVersion": "reasonix.io/plugin/v2",
  "name": "deps",
  "version": "2.0.0",
  "requires": [
    {
      "namespace": "reasonix",
      "kind": "provider",
      "id": "deepseek/v4",
      "versionRange": ">=1.0.0",
      "optional": false
    }
  ],
  "provides": [
    {
      "namespace": "plugin/deps",
      "kind": "provider",
      "id": "fake/x",
      "version": "1.0.0",
      "schemaHash": "sha256:abc"
    }
  ]
}`)
	pkg, _, err := ParseDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Manifest.Requires) != 1 || len(pkg.Manifest.Provides) != 1 {
		t.Fatalf("requires/provides = %d/%d", len(pkg.Manifest.Requires), len(pkg.Manifest.Provides))
	}
	if err := pkg.ProvidesCapabilities()[0].Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestManifestV2ProviderRequiresSchemaHash(t *testing.T) {
	root := t.TempDir()
	writeV2Plugin(t, root, `{
  "apiVersion": "reasonix.io/plugin/v2",
  "name": "bad",
  "provides": [
    {"namespace": "plugin/bad", "kind": "provider", "id": "x", "version": "1.0.0"}
  ]
}`)
	_, _, err := ParseDir(root)
	if err == nil || !strings.Contains(err.Error(), "schemaHash") {
		t.Fatalf("err = %v, want schemaHash", err)
	}
}

func TestMigrateManifestToV2(t *testing.T) {
	root := t.TempDir()
	v1 := `{
  "apiVersion": "reasonix.io/plugin/v1",
  "name": "oldplug",
  "version": "1.2.3",
  "runtime": {
    "command": "./bin/run",
    "capabilities": ["interceptors"]
  }
}`
	if err := os.WriteFile(filepath.Join(root, NativeManifest), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ParseNativeForMigrate(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := MigrateManifestToV2(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMigratedManifestV2(root, data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, NativeManifest+".v1.bak")); err != nil {
		t.Fatal(err)
	}
	got, _, err := ParseDir(root)
	if err != nil {
		t.Fatalf("parse migrated: %v", err)
	}
	if got.Manifest.APIVersion != ManifestAPIVersionV2 || got.Manifest.Name != "oldplug" {
		t.Fatalf("migrated = %+v", got.Manifest)
	}
}

func TestMigrateProvidersRequiresExplicitProvides(t *testing.T) {
	pkg := Package{ManifestKind: "reasonix", Manifest: Manifest{
		Name: "p", Version: "1.0.0",
		Runtime: &RuntimeSpec{Command: "./x", Capabilities: []string{"providers"}},
	}}
	if _, err := MigrateManifestToV2(pkg); err == nil || !strings.Contains(err.Error(), "cannot infer schemaHash") {
		t.Fatalf("err = %v", err)
	}
}

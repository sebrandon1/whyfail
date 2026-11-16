package whyfail

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sebrandon1/whyfail/internal/claudecfg"
)

func TestPluginFSHasEveryAsset(t *testing.T) {
	// The .claude-plugin manifest lives under a dot-directory, which a plain
	// //go:embed would skip silently. This test is the only thing that notices.
	want := []string{
		".claude-plugin/plugin.json",
		"hooks/hooks.json",
		"commands/prow-run.md",
		"commands/prow-job.md",
		"commands/prow-pr.md",
		"skills/prow-ci/SKILL.md",
	}
	for _, name := range want {
		if _, err := fs.Stat(PluginFS, filepath.ToSlash(filepath.Join("plugin", name))); err != nil {
			t.Errorf("embedded plugin is missing %s: %v", name, err)
		}
	}
}

func TestEmbeddedHooksMatchTheInstaller(t *testing.T) {
	data, err := fs.ReadFile(PluginFS, "plugin/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks struct {
			PreToolUse []claudecfg.HookEntry `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("hooks.json: %v", err)
	}
	if got, want := claudecfg.WhyfailHooks(), doc.Hooks.PreToolUse; !reflect.DeepEqual(got, want) {
		t.Errorf("the installer writes %+v, but the plugin declares %+v", got, want)
	}
}

func TestEmbeddedManifestAgreesWithTheMarketplace(t *testing.T) {
	data, err := fs.ReadFile(PluginFS, "plugin/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if manifest.Name != claudecfg.Marketplace {
		t.Errorf("plugin name = %q, want %q", manifest.Name, claudecfg.Marketplace)
	}

	// marketplace.json is not embedded: it is the entry point users add, so it
	// must keep pointing at plugin/ in the source tree.
	marketplace, err := os.ReadFile(filepath.Join(".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatalf("marketplace.json: %v", err)
	}
	var m struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(marketplace, &m); err != nil {
		t.Fatalf("marketplace.json: %v", err)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("marketplace lists %d plugins, want 1", len(m.Plugins))
	}
	if m.Plugins[0].Source != "./plugin" {
		t.Errorf("marketplace source = %q, want ./plugin", m.Plugins[0].Source)
	}
}

package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// recorder is a fake claude CLI: it records argv and replays canned answers.
type recorder struct {
	plugins string   // JSON for `plugin list --json`
	calls   []string // each call, space-joined
	fail    string   // a substring of the call that should fail
	missing bool     // LookPath fails
}

func (r *recorder) exec() Exec {
	return Exec{
		Look: func(string) (string, error) {
			if r.missing {
				return "", errors.New("not found")
			}
			return "/usr/bin/claude", nil
		},
		Out: func(args ...string) ([]byte, error) {
			r.calls = append(r.calls, strings.Join(args, " "))
			if r.plugins == "" {
				return nil, errors.New("no output")
			}
			return []byte(r.plugins), nil
		},
		Run: func(args ...string) error {
			call := strings.Join(args, " ")
			r.calls = append(r.calls, call)
			if r.fail != "" && strings.Contains(call, r.fail) {
				return errors.New("boom")
			}
			return nil
		},
	}
}

func newInstaller(t *testing.T, rec *recorder) *Installer {
	t.Helper()
	home := t.TempDir()
	paths, err := ForScope(ScopeUser, "", home)
	if err != nil {
		t.Fatal(err)
	}
	return &Installer{
		Scope:   ScopeUser,
		Paths:   paths,
		From:    "sebrandon1/whyfail",
		FS:      testFS(),
		Exec:    rec.exec(),
		Binary:  "/usr/bin/whyfail",
		MCPPath: filepath.Join(home, ".claude.json"),
	}
}

func TestInstallPluginWhenAlreadyEnabled(t *testing.T) {
	rec := &recorder{plugins: `[{"id":"whyfail@whyfail","enabled":true}]`}
	in := newInstaller(t, rec)
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("ran %v, want only the plugin list query", rec.calls)
	}
	if steps[0].Status != StatusSkipped || !strings.Contains(steps[0].Detail, "already installed") {
		t.Errorf("plugin step = %+v", steps[0])
	}
}

func TestInstallPluginDisabledIsEnabled(t *testing.T) {
	rec := &recorder{plugins: `[{"id":"whyfail@whyfail","enabled":false}]`}
	in := newInstaller(t, rec)
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{"plugin list --json", "plugin enable whyfail@whyfail"}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("calls = %v, want %v", rec.calls, want)
	}
	if steps[0].Status != StatusOK {
		t.Errorf("plugin step = %+v", steps[0])
	}
}

func TestInstallPluginFromScratch(t *testing.T) {
	rec := &recorder{plugins: `[]`}
	in := newInstaller(t, rec)
	in.Yes = true
	if _, err := in.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{
		"plugin list --json",
		"plugin install whyfail --marketplace sebrandon1/whyfail -s user -y",
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("calls = %v, want %v", rec.calls, want)
	}
}

func TestInstallNeedsClaudeUnlessHooksOnly(t *testing.T) {
	rec := &recorder{missing: true}
	in := newInstaller(t, rec)
	if _, err := in.Install(); err == nil {
		t.Fatal("expected an error when claude is missing")
	} else if !strings.Contains(err.Error(), "claude not found") {
		t.Errorf("error = %v", err)
	}

	in = newInstaller(t, rec)
	in.HooksOnly = true
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("hooks-only should not need claude: %v", err)
	}
	if steps[0].Status != StatusOK {
		t.Errorf("hooks step = %+v", steps[0])
	}
}

func TestInstallHooksOnlyRefusesWhenPluginEnabled(t *testing.T) {
	rec := &recorder{}
	in := newInstaller(t, rec)
	in.HooksOnly = true
	if err := os.MkdirAll(filepath.Dir(in.Paths.Settings), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"enabledPlugins": {"whyfail@whyfail": true}}`
	if err := os.WriteFile(in.Paths.Settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := in.Install()
	if err == nil || !strings.Contains(err.Error(), "--uninstall") {
		t.Fatalf("error = %v, want guidance mentioning --uninstall", err)
	}
	after, err := os.ReadFile(in.Paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Error("settings were written despite the refusal")
	}
}

func TestInstallHooksOnlyWritesHooksAndAssets(t *testing.T) {
	rec := &recorder{missing: true}
	in := newInstaller(t, rec)
	in.HooksOnly = true
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("hooks-only ran claude: %v", rec.calls)
	}
	if steps[0].Status != StatusOK {
		t.Errorf("hooks step = %+v", steps[0])
	}
	state, err := ReadState(in.Paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Hooks {
		t.Error("settings.json does not contain the whyfail hooks")
	}
	for _, name := range []string{"prow-run.md", "prow-job.md"} {
		if _, err := os.Stat(filepath.Join(in.Paths.Commands, name)); err != nil {
			t.Errorf("command %s not installed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(in.Paths.Skills, "prow-ci", "SKILL.md")); err != nil {
		t.Errorf("skill not installed: %v", err)
	}

	// A second run changes nothing.
	again, err := in.Install()
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if again[0].Status != StatusSkipped {
		t.Errorf("second run = %+v, want skipped", again[0])
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	rec := &recorder{plugins: `[]`}
	in := newInstaller(t, rec)
	in.DryRun = true
	in.MCP = true
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("dry run ran %v", rec.calls)
	}
	for _, s := range steps {
		// The binary row reports the PATH check, which is a fact about the
		// machine rather than an action, so it is always ok.
		if s.Name == "binary" {
			continue
		}
		if s.Status != StatusPlanned && s.Status != StatusSkipped {
			t.Errorf("step %q status = %q", s.Name, s.Status)
		}
	}
	if _, err := os.Stat(in.Paths.Settings); !os.IsNotExist(err) {
		t.Error("dry run created settings.json")
	}
}

func TestUninstallRoundTrip(t *testing.T) {
	rec := &recorder{missing: true}
	in := newInstaller(t, rec)
	in.HooksOnly = true

	const original = `{"model": "opus"}`
	if err := os.MkdirAll(filepath.Dir(in.Paths.Settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.Paths.Settings, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	in.Uninstall = true
	if _, err := in.Install(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	after, err := os.ReadFile(in.Paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mustJSON(t, after), mustJSON(t, []byte(original))) {
		t.Errorf("settings after uninstall:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(in.Paths.Commands, "prow-run.md")); !os.IsNotExist(err) {
		t.Error("commands were not removed")
	}
	// A second uninstall is a clean no-op.
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if steps[0].Status != StatusSkipped {
		t.Errorf("second uninstall = %+v", steps[0])
	}
}

func TestInstallMCP(t *testing.T) {
	rec := &recorder{plugins: `[{"id":"whyfail@whyfail","enabled":true}]`}
	in := newInstaller(t, rec)
	in.MCP = true
	steps, err := in.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	found := false
	for _, s := range steps {
		if s.Name == "mcp" {
			found = true
			if s.Status != StatusOK {
				t.Errorf("mcp step = %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("no mcp step in %+v", steps)
	}
	want := []string{"plugin list --json", "mcp add whyfail -s user -- whyfail mcp"}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("calls = %v, want %v", rec.calls, want)
	}

	// Registered already: the configuration file is enough to tell.
	if err := os.WriteFile(in.MCPPath, []byte(`{"mcpServers": {"whyfail": {"command": "whyfail"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil
	steps, err = in.Install()
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	for _, s := range steps {
		if s.Name == "mcp" && s.Status != StatusSkipped {
			t.Errorf("mcp step = %+v, want skipped", s)
		}
	}
}

func TestInstallReportsTheFirstFailure(t *testing.T) {
	rec := &recorder{plugins: `[]`, fail: "plugin install"}
	in := newInstaller(t, rec)
	in.MCP = true
	steps, err := in.Install()
	if err == nil {
		t.Fatal("expected the install failure to surface")
	}
	if len(steps) < 3 {
		t.Errorf("steps = %+v, want the remaining steps reported too", steps)
	}
	if steps[0].Status != StatusFailed {
		t.Errorf("plugin step = %+v", steps[0])
	}
}

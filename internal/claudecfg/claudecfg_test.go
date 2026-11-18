package claudecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestForScope(t *testing.T) {
	user, err := ForScope(ScopeUser, "/work", "/home/u")
	if err != nil {
		t.Fatalf("user scope: %v", err)
	}
	if want := filepath.Join("/home/u", ".claude", "settings.json"); user.Settings != want {
		t.Errorf("settings = %q, want %q", user.Settings, want)
	}
	proj, err := ForScope(ScopeProject, "/work", "/home/u")
	if err != nil {
		t.Fatalf("project scope: %v", err)
	}
	if want := filepath.Join("/work", ".claude", "settings.json"); proj.Settings != want {
		t.Errorf("settings = %q, want %q", proj.Settings, want)
	}
	if _, err := ForScope(Scope("nope"), "/w", "/h"); err == nil {
		t.Error("expected an error for an unknown scope")
	}
}

func TestForScopeHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	got, err := ForScope(ScopeUser, "", "/home/u")
	if err != nil {
		t.Fatalf("ForScope: %v", err)
	}
	if want := filepath.Join("/custom/claude", "settings.json"); got.Settings != want {
		t.Errorf("settings = %q, want %q", got.Settings, want)
	}
}

// A project registration lives in the repository, never in $HOME, and is what
// doctor reports when both files name the server.
func TestMCPServersPathForAndRegisteredScope(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	if got, want := MCPServersPathFor(ScopeUser, cwd, home), MCPServersPath(home); got != want {
		t.Errorf("user path = %q, want %q", got, want)
	}
	proj := MCPServersPathFor(ScopeProject, cwd, home)
	if proj != filepath.Join(cwd, ProjectMCPFile) {
		t.Fatalf("project path = %q, want the repository's own file", proj)
	}
	if got := MCPServersPathFor(ScopeProject, "", home); got != "" {
		t.Errorf("project path with no cwd = %q, want empty", got)
	}

	if got := MCPRegisteredScope(cwd, home); got != ScopeNone {
		t.Errorf("scope = %q, want none while neither file exists", got)
	}

	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"mcpServers": {"`+Marketplace+`": {}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(MCPServersPath(home))
	if got := MCPRegisteredScope(cwd, home); got != ScopeUser {
		t.Errorf("scope = %q, want user", got)
	}
	write(proj)
	if got := MCPRegisteredScope(cwd, home); got != ScopeProject {
		t.Errorf("scope = %q, want project to win in its own directory", got)
	}
}

func TestReadState(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    State
		wantStr string
	}{
		{
			name:    "missing file",
			body:    "",
			want:    State{},
			wantStr: "not installed (run whyfail init)",
		},
		{
			name:    "plugin enabled",
			body:    `{"enabledPlugins": {"whyfail@whyfail": true}}`,
			want:    State{Known: true, Plugin: true},
			wantStr: "installed (whyfail@whyfail, user)",
		},
		{
			name:    "plugin disabled",
			body:    `{"enabledPlugins": {"whyfail@whyfail": false}}`,
			want:    State{Known: true},
			wantStr: "installed but disabled (run: claude plugin enable whyfail@whyfail)",
		},
		{
			name:    "hooks only",
			body:    `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "whyfail hook bash"}]}]}}`,
			want:    State{Hooks: true},
			wantStr: "hooks-only",
		},
		{
			name:    "both installed",
			body:    `{"enabledPlugins": {"whyfail@whyfail": true}, "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "whyfail hook bash"}]}]}}`,
			want:    State{Known: true, Plugin: true, Hooks: true},
			wantStr: "duplicated: plugin and settings.json hooks (hooks run twice)",
		},
		{
			name:    "another tool's hooks",
			body:    `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk hook claude"}]}]}}`,
			want:    State{},
			wantStr: "not installed (run whyfail init)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if c.body != "" {
				if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadState(path)
			if err != nil {
				t.Fatalf("ReadState: %v", err)
			}
			if got != c.want {
				t.Errorf("state = %+v, want %+v", got, c.want)
			}
			if s := got.Install(); s != c.wantStr {
				t.Errorf("Install() = %q, want %q", s, c.wantStr)
			}
		})
	}
}

func TestReadStateRejectsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{nope`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(path); err == nil {
		t.Error("expected an error for malformed settings")
	}
}

func TestWriteAtomicPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".whyfail-") {
			t.Errorf("temporary file %q was left behind", e.Name())
		}
	}
}

func TestWriteAtomicThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteAtomic(link, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("target holds %q", data)
	}
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"plugin/commands/prow-run.md":    {Data: []byte("run")},
		"plugin/commands/prow-job.md":    {Data: []byte("job")},
		"plugin/skills/prow-ci/SKILL.md": {Data: []byte("skill")},
	}
}

func TestInstallAssetsIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	written, err := InstallAssets(testFS(), "plugin/commands", filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatalf("InstallAssets: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %d files, want 2", len(written))
	}
	again, err := InstallAssets(testFS(), "plugin/commands", filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatalf("second InstallAssets: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second run rewrote %v", again)
	}
	got, err := os.ReadFile(filepath.Join(dir, "commands", "prow-run.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "run" {
		t.Errorf("content = %q", got)
	}
}

func TestRemoveAssetsKeepsEditedFiles(t *testing.T) {
	dir := t.TempDir()
	cmds := filepath.Join(dir, "commands")
	skills := filepath.Join(dir, "skills")
	if _, err := InstallAssets(testFS(), "plugin/commands", cmds); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallAssets(testFS(), "plugin/skills", skills); err != nil {
		t.Fatal(err)
	}
	// The user's edit makes this file theirs.
	edited := filepath.Join(cmds, "prow-job.md")
	if err := os.WriteFile(edited, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveAssets(testFS(), "plugin/commands", cmds)
	if err != nil {
		t.Fatalf("RemoveAssets: %v", err)
	}
	if len(removed) != 1 {
		t.Errorf("removed %v, want only the untouched command", removed)
	}
	if _, err := os.Stat(edited); err != nil {
		t.Errorf("an edited file was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cmds, "prow-run.md")); !os.IsNotExist(err) {
		t.Error("an untouched command survived")
	}
	// The directory is left in place because the edited file is still there.
	if _, err := os.Stat(cmds); err != nil {
		t.Errorf("commands dir was removed while non-empty: %v", err)
	}

	if _, err := RemoveAssets(testFS(), "plugin/skills", skills); err != nil {
		t.Fatalf("RemoveAssets skills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "prow-ci")); !os.IsNotExist(err) {
		t.Error("the empty prow-ci directory survived")
	}
}

// Package claudecfg reads and writes Claude Code's settings.json and installs
// the plugin's commands and skill. It exists so `whyfail init` and `whyfail
// doctor` can share one definition of what "installed" means, and so both can
// be tested against a temporary home directory instead of the real one.
package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PluginID is how Claude Code names an installed plugin: name@marketplace.
const PluginID = "whyfail@whyfail"

// Marketplace is the marketplace name declared in .claude-plugin/marketplace.json.
const Marketplace = "whyfail"

// HookPrefix identifies a hook command as one of ours. Both the plugin's
// hooks.json and a --hooks-only install produce commands under it.
const HookPrefix = "whyfail hook "

// Scope is where Claude Code reads configuration from.
type Scope string

const (
	// ScopeUser is ~/.claude, shared by every project.
	ScopeUser Scope = "user"
	// ScopeProject is ./.claude, belonging to one repository.
	ScopeProject Scope = "project"
)

// Paths is the set of locations a scope owns.
type Paths struct {
	Settings string
	Commands string
	Skills   string
}

// ConfigDir is the user-scope configuration directory. $CLAUDE_CONFIG_DIR
// wins when Claude Code has been pointed somewhere other than ~/.claude.
func ConfigDir(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".claude")
}

// ForScope resolves a scope's paths under home or cwd. Project scope is
// relative to cwd so `whyfail init --project` writes into the repository the
// user is standing in.
func ForScope(s Scope, cwd, home string) (Paths, error) {
	var base string
	switch s {
	case ScopeProject:
		if cwd == "" {
			return Paths{}, errors.New("project scope needs a working directory")
		}
		base = filepath.Join(cwd, ".claude")
	case ScopeUser:
		if home == "" {
			return Paths{}, errors.New("user scope needs a home directory")
		}
		base = ConfigDir(home)
	default:
		return Paths{}, fmt.Errorf("unknown scope %q", s)
	}
	return Paths{
		Settings: filepath.Join(base, "settings.json"),
		Commands: filepath.Join(base, "commands"),
		Skills:   filepath.Join(base, "skills"),
	}, nil
}

// State is what is already installed, as far as settings.json can tell. It is
// read offline: doctor must stay fast, and init uses it to stay idempotent.
type State struct {
	// Known is true when enabledPlugins mentions whyfail at all, which is how
	// an explicit false is told apart from never installed.
	Known bool
	// Plugin is whyfail@whyfail present and enabled.
	Plugin bool
	// Hooks is a whyfail PreToolUse hook present in settings.json, the shape a
	// --hooks-only install leaves behind.
	Hooks bool
}

// Install describes the integration in one phrase, for doctor.
func (s State) Install() string {
	switch {
	case s.Plugin && s.Hooks:
		return "duplicated: plugin and settings.json hooks (hooks run twice)"
	case s.Plugin:
		return "installed (" + PluginID + ", user)"
	case s.Known:
		return "installed but disabled (run: claude plugin enable " + PluginID + ")"
	case s.Hooks:
		return "hooks-only"
	default:
		return "not installed (run whyfail init)"
	}
}

// ReadState reports what settings.json says. A missing file is not an error:
// it means nothing is installed yet.
func ReadState(settingsPath string) (State, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}
	if len(data) == 0 {
		return State{}, nil
	}
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return State{}, err
	}
	st := State{}
	if enabled, ok := doc.EnabledPlugins[PluginID]; ok {
		st.Known = true
		st.Plugin = enabled
	}
	for _, e := range doc.Hooks.PreToolUse {
		for _, h := range e.Hooks {
			if hasHookPrefix(h.Command) {
				st.Hooks = true
			}
		}
	}
	return st, nil
}

func hasHookPrefix(cmd string) bool {
	return len(cmd) >= len(HookPrefix) && cmd[:len(HookPrefix)] == HookPrefix
}

// MCPServersPath is the file holding user-scope MCP servers. It is read
// directly rather than through `claude mcp get`, which health-checks the
// server and would make init wait on a connection.
func MCPServersPath(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// MCPRegistered reports whether name is configured as a user-scope MCP server.
// A missing or unreadable file means "not registered".
func MCPRegistered(path, name string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	_, ok := doc.MCPServers[name]
	return ok
}

// WriteAtomic replaces path with data, so an interrupted run cannot leave a
// truncated settings file. An existing file keeps its mode, and a symlinked
// settings.json is written through rather than replaced by a regular file.
func WriteAtomic(path string, data []byte) error {
	target := resolveTarget(path)
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".whyfail-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; restore the mode the target expects.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

// resolveTarget follows a symlinked settings file to the file it points at, so
// the rename replaces the contents rather than the link.
func resolveTarget(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(r, filepath.Base(path))
	}
	return path
}

// ReadFile reads a settings file, treating a missing file as empty.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte("{}"), nil
		}
		return nil, err
	}
	if len(trimSpace(data)) == 0 {
		return []byte("{}"), nil
	}
	return data, nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

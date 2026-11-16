package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Exec is the seam to the `claude` CLI so tests never spawn a process.
type Exec struct {
	// Look is exec.LookPath.
	Look func(string) (string, error)
	// Out runs claude and returns its stdout, for machine-readable reads.
	Out func(args ...string) ([]byte, error)
	// Run runs claude with the caller's stdin, stdout and stderr, so a trust
	// prompt can be answered.
	Run func(args ...string) error
}

// Step statuses.
const (
	StatusOK      = "ok"
	StatusPlanned = "planned"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
)

// Step is one row of the install report.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Installer performs one `whyfail init` run.
type Installer struct {
	Scope     Scope
	Paths     Paths
	From      string
	HooksOnly bool
	MCP       bool
	Uninstall bool
	DryRun    bool
	Yes       bool
	FS        fs.FS
	Exec      Exec
	// Binary is the PATH check already rendered by the caller.
	Binary string
	// MCPPath is the user-scope MCP configuration file, read to keep a second
	// `init --mcp` from re-registering the server.
	MCPPath string
}

// commandsSource and skillsSource are where the embedded assets live.
const (
	commandsSource = "plugin/commands"
	skillsSource   = "plugin/skills"
)

// Install applies (or with Uninstall, reverses) the Claude Code integration.
// It runs every step it can and returns them all, so a partial failure is
// visible rather than silent; the error names the first one that failed.
func (in *Installer) Install() ([]Step, error) {
	if in.Uninstall {
		return in.uninstall()
	}
	return in.install()
}

func (in *Installer) install() ([]Step, error) {
	var steps []Step
	var firstErr error
	add := func(s Step, err error) {
		steps = append(steps, s)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if in.HooksOnly {
		state, err := ReadState(in.Paths.Settings)
		if err == nil && state.Plugin {
			err = fmt.Errorf("plugin %s is installed and enabled; its hooks would run twice. Run `whyfail init --uninstall` first, or drop --hooks-only", PluginID)
		}
		if err != nil {
			return append(steps, Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}), err
		}
		st := in.installHooks()
		var hooksErr error
		if st.Status == StatusFailed {
			hooksErr = errors.New(st.Detail)
		}
		add(st, hooksErr)
	} else {
		st, err := in.installPlugin()
		add(st, err)
		// Both paths installed is the one self-inflicted mistake worth calling
		// out: Claude Code would run the plugin's hooks and these.
		if state, sErr := ReadState(in.Paths.Settings); sErr == nil && state.Hooks {
			steps = append(steps, Step{
				Name:   "settings",
				Status: StatusSkipped,
				Detail: "whyfail hooks are also in settings.json; Claude Code will run them as well as the plugin",
			})
		}
	}

	if in.MCP {
		st, err := in.installMCP()
		add(st, err)
	}

	steps = append(steps, Step{Name: "binary", Status: binaryStepStatus(in.Binary), Detail: in.Binary})
	if firstErr != nil {
		return steps, firstErr
	}
	return steps, nil
}

func (in *Installer) installPlugin() (Step, error) {
	if _, err := in.Exec.Look("claude"); err != nil {
		return Step{
			Name:   "plugin",
			Status: StatusFailed,
			Detail: "claude not on PATH; use --hooks-only to write the hooks into settings.json instead",
		}, errors.New("claude not found on PATH")
	}

	if data, err := in.Exec.Out("plugin", "list", "--json"); err == nil {
		var installed []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if json.Unmarshal(data, &installed) == nil {
			for _, p := range installed {
				if p.ID != PluginID {
					continue
				}
				if p.Enabled {
					return Step{Name: "plugin", Status: StatusSkipped, Detail: "already installed and enabled"}, nil
				}
				args := []string{"plugin", "enable", PluginID}
				if in.DryRun {
					return Step{Name: "plugin", Status: StatusPlanned, Detail: "claude " + strings.Join(args, " ")}, nil
				}
				if err := in.Exec.Run(args...); err != nil {
					return Step{Name: "plugin", Status: StatusFailed, Detail: err.Error()}, err
				}
				return Step{Name: "plugin", Status: StatusOK, Detail: "enabled"}, nil
			}
		}
	}

	args := []string{"plugin", "install", Marketplace, "--marketplace", in.From, "-s", string(in.Scope)}
	if in.Yes {
		args = append(args, "-y")
	}
	if in.DryRun {
		return Step{Name: "plugin", Status: StatusPlanned, Detail: "claude " + strings.Join(args, " ")}, nil
	}
	if err := in.Exec.Run(args...); err != nil {
		return Step{Name: "plugin", Status: StatusFailed, Detail: err.Error()}, err
	}
	return Step{Name: "plugin", Status: StatusOK, Detail: "installed from " + in.From}, nil
}

func (in *Installer) installMCP() (Step, error) {
	if _, err := in.Exec.Look("claude"); err != nil {
		return Step{Name: "mcp", Status: StatusSkipped, Detail: "claude not on PATH"}, nil
	}
	if MCPRegistered(in.MCPPath, Marketplace) {
		return Step{Name: "mcp", Status: StatusSkipped, Detail: "already registered"}, nil
	}
	args := []string{"mcp", "add", Marketplace, "-s", string(in.Scope), "--", "whyfail", "mcp"}
	if in.DryRun {
		return Step{Name: "mcp", Status: StatusPlanned, Detail: "claude " + strings.Join(args, " ")}, nil
	}
	if err := in.Exec.Run(args...); err != nil {
		return Step{Name: "mcp", Status: StatusFailed, Detail: err.Error()}, err
	}
	return Step{Name: "mcp", Status: StatusOK, Detail: "registered (" + string(in.Scope) + " scope)"}, nil
}

// installHooks writes the two PreToolUse entries and copies the commands and
// skill, without involving the plugin system at all.
func (in *Installer) installHooks() Step {
	current, err := ReadFile(in.Paths.Settings)
	if err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	}
	merged, changed, err := MergeHooks(current, WhyfailHooks())
	if err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	}

	var detail string
	switch {
	case in.DryRun && changed:
		detail = "would add Bash and WebFetch PreToolUse hooks to " + in.Paths.Settings
	case in.DryRun:
		detail = "hooks already present"
	case changed:
		if err := WriteAtomic(in.Paths.Settings, merged); err != nil {
			return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
		}
		detail = "added Bash and WebFetch PreToolUse hooks to " + in.Paths.Settings
	default:
		detail = "hooks already present"
	}

	if in.DryRun {
		return Step{Name: "hooks", Status: StatusPlanned, Detail: detail}
	}
	if written, err := InstallAssets(in.FS, commandsSource, in.Paths.Commands); err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	} else if n := len(written); n > 0 {
		detail += fmt.Sprintf("; %d command(s)", n)
	}
	if written, err := InstallAssets(in.FS, skillsSource, in.Paths.Skills); err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	} else if n := len(written); n > 0 {
		detail += fmt.Sprintf("; %d skill file(s)", n)
	}
	st := StatusOK
	if !changed {
		st = StatusSkipped
	}
	return Step{Name: "hooks", Status: st, Detail: detail}
}

func (in *Installer) uninstall() ([]Step, error) {
	var steps []Step
	var firstErr error
	add := func(s Step) {
		steps = append(steps, s)
		if s.Status == StatusFailed && firstErr == nil {
			firstErr = errors.New(s.Detail)
		}
	}

	if !in.HooksOnly {
		if _, err := in.Exec.Look("claude"); err != nil {
			add(Step{Name: "plugin", Status: StatusSkipped, Detail: "claude not on PATH"})
		} else if in.DryRun {
			add(Step{Name: "plugin", Status: StatusPlanned, Detail: "claude plugin uninstall " + PluginID})
		} else {
			// Every removal tolerates "not there", which keeps uninstall
			// idempotent.
			_ = in.Exec.Run("mcp", "remove", Marketplace, "-s", string(in.Scope))
			if err := in.Exec.Run("plugin", "uninstall", PluginID); err != nil {
				add(Step{Name: "plugin", Status: StatusSkipped, Detail: "not installed"})
			} else {
				add(Step{Name: "plugin", Status: StatusOK, Detail: "uninstalled " + PluginID})
			}
			_ = in.Exec.Run("plugin", "marketplace", "remove", Marketplace)
		}
	}

	add(in.removeHooks())
	steps = append(steps, Step{Name: "binary", Status: StatusOK, Detail: in.Binary})
	if firstErr != nil {
		return steps, firstErr
	}
	return steps, nil
}

func (in *Installer) removeHooks() Step {
	current, err := os.ReadFile(in.Paths.Settings)
	if err != nil {
		if os.IsNotExist(err) {
			return Step{Name: "hooks", Status: StatusSkipped, Detail: "no settings.json"}
		}
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	}
	cleaned, changed, err := RemoveHooks(current, IsWhyfailHook)
	if err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	}
	if !changed {
		return Step{Name: "hooks", Status: StatusSkipped, Detail: "no whyfail hooks found"}
	}
	if in.DryRun {
		return Step{Name: "hooks", Status: StatusPlanned, Detail: "would remove the whyfail PreToolUse hooks"}
	}
	if err := WriteAtomic(in.Paths.Settings, cleaned); err != nil {
		return Step{Name: "hooks", Status: StatusFailed, Detail: err.Error()}
	}
	cmds, _ := RemoveAssets(in.FS, commandsSource, in.Paths.Commands)
	skills, _ := RemoveAssets(in.FS, skillsSource, in.Paths.Skills)
	return Step{
		Name:   "hooks",
		Status: StatusOK,
		Detail: fmt.Sprintf("removed the whyfail hooks, %d command(s) and %d skill file(s)", len(cmds), len(skills)),
	}
}

// binaryStepStatus turns the PATH check into a row status. A missing or stale
// binary is a warning, not a failure: the config still installed correctly.
func binaryStepStatus(detail string) string {
	if strings.HasPrefix(detail, "not on PATH") || strings.Contains(detail, "possibly stale") {
		return StatusSkipped
	}
	return StatusOK
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	wf "github.com/sebrandon1/whyfail"
	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/claudecfg"
	"github.com/sebrandon1/whyfail/internal/term"
)

// initOut is the --json document for `whyfail init`.
type initOut struct {
	Schema   string           `json:"schema"`
	Scope    string           `json:"scope"`
	Mode     string           `json:"mode"`
	DryRun   bool             `json:"dry_run"`
	From     string           `json:"from"`
	Settings string           `json:"settings"`
	Steps    []claudecfg.Step `json:"steps"`
	OK       bool             `json:"ok"`
}

// initOptions is the flag set, separated from the command so runInit can be
// driven without a cobra command or the real environment.
type initOptions struct {
	global, project   bool
	hooksOnly, mcp    bool
	dryRun, uninstall bool
	yes               bool
	from              string
}

// initDeps is everything runInit would otherwise read from the process:
// filesystem paths, the claude CLI, the embedded assets and the binary check.
// Tests replace each one.
type initDeps struct {
	userHome func() (string, error)
	getwd    func() (string, error)
	exec     claudecfg.Exec
	fs       fs.FS
	binary   func() string
	stdout   io.Writer
}

func newInitCmd(g *globals) *cobra.Command {
	var opts initOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install the Claude Code integration",
		Long: `Install whyfail's Claude Code integration.

By default the plugin is installed through the claude CLI, which brings the
PreToolUse hooks, the /prow-run, /prow-job and /prow-pr commands and the
prow-ci skill. --hooks-only skips the plugin system and writes the same hooks
into settings.json, copying the commands and skill into place directly.

Nothing here is recorded in the savings statistics.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(g, opts, initDeps{
				userHome: os.UserHomeDir,
				getwd:    os.Getwd,
				exec:     claudeExec(g),
				fs:       wf.PluginFS,
				binary:   binaryStatus,
				stdout:   os.Stdout,
			})
		},
	}
	cmd.Flags().BoolVar(&opts.global, "global", false, "install for the user (~/.claude); the default")
	cmd.Flags().BoolVar(&opts.project, "project", false, "install for the current repository (./.claude)")
	cmd.Flags().BoolVar(&opts.hooksOnly, "hooks-only", false, "write the hooks into settings.json instead of using the plugin system")
	cmd.Flags().BoolVar(&opts.mcp, "mcp", false, "also register the MCP server")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "print every step without changing anything")
	cmd.Flags().BoolVar(&opts.uninstall, "uninstall", false, "remove the integration")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "accept the claude CLI's prompts (needed when not on a TTY)")
	cmd.Flags().StringVar(&opts.from, "from", "sebrandon1/whyfail", "marketplace source: owner/repo, URL, or path")
	return cmd
}

// runInit performs one init run. It reports a failure to locate the home or
// working directory rather than passing an empty path into the installer: this
// command writes the user's Claude configuration, so a wrong path must be an
// error, not a silent install somewhere else.
func runInit(g *globals, opts initOptions, deps initDeps) error {
	if opts.global && opts.project {
		return errors.New("choose one of --global or --project")
	}
	if strings.TrimSpace(opts.from) == "" {
		return errors.New("--from cannot be empty")
	}
	scope := claudecfg.ScopeUser
	if opts.project {
		scope = claudecfg.ScopeProject
	}
	home, err := deps.userHome()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}
	cwd, err := deps.getwd()
	if err != nil {
		return fmt.Errorf("locate working directory: %w", err)
	}
	paths, err := claudecfg.ForScope(scope, cwd, home)
	if err != nil {
		return err
	}

	in := &claudecfg.Installer{
		Scope:     scope,
		Paths:     paths,
		From:      opts.from,
		HooksOnly: opts.hooksOnly,
		MCP:       opts.mcp,
		Uninstall: opts.uninstall,
		DryRun:    opts.dryRun,
		Yes:       opts.yes,
		FS:        deps.fs,
		Exec:      deps.exec,
		Binary:    deps.binary(),
		MCPPath:   claudecfg.MCPServersPathFor(scope, cwd, home),
	}
	steps, runErr := in.Install()

	mode := "plugin"
	if opts.hooksOnly {
		mode = "hooks-only"
	}
	if g.jsonOut {
		var buf bytes.Buffer
		writeJSON(&buf, initOut{
			Schema:   api.SchemaV1,
			Scope:    string(scope),
			Mode:     mode,
			DryRun:   opts.dryRun,
			From:     opts.from,
			Settings: paths.Settings,
			Steps:    steps,
			OK:       runErr == nil,
		})
		_, _ = deps.stdout.Write(buf.Bytes())
		return runErr
	}
	st := g.style()
	verb := "init"
	if opts.uninstall {
		verb = "uninstall"
	}
	fmt.Fprintln(deps.stdout, st.Bold("whyfail "+verb)+st.Dim(" ("+mode+", "+string(scope)+" scope)"))
	for _, s := range steps {
		initStepLine(deps.stdout, st, s)
	}
	return runErr
}

// initStepLine renders one row in the same gutter as doctor.
func initStepLine(w io.Writer, st term.Style, s claudecfg.Step) {
	icon := "✓"
	switch s.Status {
	case claudecfg.StatusSkipped:
		icon = "·"
	case claudecfg.StatusPlanned:
		icon = "→"
	case claudecfg.StatusFailed:
		icon = "✗"
	}
	v := icon + " " + s.Detail
	switch s.Status {
	case claudecfg.StatusFailed:
		v = st.Red(v)
	case claudecfg.StatusSkipped:
		v = st.Dim(v)
	case claudecfg.StatusPlanned:
		v = st.Yellow(v)
	}
	doctorLine(w, st, s.Name, v)
}

// claudeExec wires the installer to the claude CLI. Writes that may prompt for
// trust inherit the terminal; under --json the child's stdout is diverted to
// stderr so the JSON document stays the only thing on stdout.
func claudeExec(g *globals) claudecfg.Exec {
	return claudecfg.Exec{
		Look: exec.LookPath,
		Out: func(args ...string) ([]byte, error) {
			c := exec.Command("claude", args...)
			var out, errb bytes.Buffer
			c.Stdout = &out
			c.Stderr = &errb
			if err := c.Run(); err != nil {
				return nil, fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
			}
			return out.Bytes(), nil
		},
		Run: func(args ...string) error {
			c := exec.Command("claude", args...)
			c.Stdin = os.Stdin
			c.Stderr = os.Stderr
			var stdout io.Writer = os.Stdout
			if g.jsonOut {
				stdout = os.Stderr
			}
			c.Stdout = stdout
			if err := c.Run(); err != nil {
				return fmt.Errorf("claude %s: %w", strings.Join(args, " "), err)
			}
			return nil
		},
	}
}

// doctorClaude reports whether the integration is installed, using only
// settings.json: doctor stays offline and never launches claude.
func doctorClaude(w io.Writer, st term.Style, settingsPath string) {
	state, err := claudecfg.ReadState(settingsPath)
	var v string
	switch {
	case err != nil:
		v = st.Yellow("settings.json unreadable: " + err.Error())
	case state.Plugin || state.Hooks:
		v = st.Green(state.Install())
	default:
		v = st.Yellow(state.Install())
	}
	doctorLine(w, st, "plugin", v)
}

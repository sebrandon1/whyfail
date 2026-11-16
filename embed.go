// Package whyfail holds the assets that ship alongside the CLI binary. The
// plugin directory is embedded so `whyfail init` can install the Claude Code
// integration without needing the source tree it was built from.
package whyfail

import "embed"

// PluginFS holds the Claude Code plugin rooted at plugin/. The all: prefix is
// required: without it, go:embed skips the dot-prefixed .claude-plugin
// directory that carries the plugin manifest.
//
//go:embed all:plugin
var PluginFS embed.FS

// Package hook implements the Claude Code PreToolUse policy that redirects
// Prow and GCS fetches to whyfail.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/sebrandon1/whyfail/internal/prow"
)

// Input is the subset of the Claude Code hook payload we use.
type Input struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
		URL     string `json:"url"`
	} `json:"tool_input"`
}

// Output is the Claude Code PreToolUse hook response.
type Output struct {
	HookSpecificOutput *Specific `json:"hookSpecificOutput,omitempty"`
}

// Specific carries the decision.
type Specific struct {
	HookEventName            string         `json:"hookEventName"`
	PermissionDecision       string         `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
}

var urlRe = regexp.MustCompile(`https?://[^\s'"` + "`" + `]+`)

// Handle reads a hook payload and writes the decision.
func Handle(tool string, in io.Reader, out io.Writer) error {
	var payload Input
	if err := json.NewDecoder(in).Decode(&payload); err != nil {
		return allow(out)
	}

	switch tool {
	case "bash":
		return handleBash(payload, out)
	case "webfetch":
		return handleWebFetch(payload, out)
	}
	return allow(out)
}

// allow writes an empty decision, leaving the tool call unchanged.
func allow(out io.Writer) error {
	return json.NewEncoder(out).Encode(Output{})
}

func handleBash(payload Input, out io.Writer) error {
	cmd := strings.TrimSpace(payload.ToolInput.Command)
	if cmd == "" || strings.Contains(cmd, "whyfail") {
		return allow(out)
	}
	if !isSimpleFetch(cmd) {
		return allow(out)
	}
	u := urlRe.FindString(cmd)
	if u == "" {
		return allow(out)
	}
	replacement, ok := rewrite(u)
	if !ok {
		return allow(out)
	}
	return json.NewEncoder(out).Encode(Output{HookSpecificOutput: &Specific{
		HookEventName:      "PreToolUse",
		PermissionDecision: "allow",
		UpdatedInput:       map[string]any{"command": replacement},
	}})
}

func handleWebFetch(payload Input, out io.Writer) error {
	u := payload.ToolInput.URL
	replacement, ok := rewrite(u)
	if !ok {
		return allow(out)
	}
	return json.NewEncoder(out).Encode(Output{HookSpecificOutput: &Specific{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: fmt.Sprintf("this host is handled better by whyfail; run: %s", replacement),
	}})
}

// isSimpleFetch reports whether cmd is a bare curl/wget of one URL, so the
// rewrite cannot drop pipes, redirections, or extra arguments.
func isSimpleFetch(cmd string) bool {
	if strings.ContainsAny(cmd, "|><;&") {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	if fields[0] != "curl" && fields[0] != "wget" {
		return false
	}
	n := 0
	for _, f := range fields[1:] {
		f = strings.Trim(f, `'"`)
		if strings.HasPrefix(f, "-") {
			continue
		}
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			n++
			continue
		}
		// An argument that is not a flag or URL means this is not a simple fetch.
		return false
	}
	return n == 1
}

// rewrite maps a CI URL to the equivalent whyfail invocation.
func rewrite(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if _, err := prow.ParseRef(raw); err == nil {
		return "whyfail run " + quote(raw), true
	}
	if bucket, _, err := prow.ParseArtifactURL(raw); err == nil && bucket != "" {
		return "whyfail artifact " + quote(raw), true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch u.Host {
	case "search.ci.openshift.org", "search.dptools.openshift.org":
		q := u.Query().Get("search")
		if q == "" {
			return "", false
		}
		return "whyfail search " + quote(q), true
	}
	return "", false
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

package claudecfg

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// settingsFixture mirrors the shape of a real ~/.claude/settings.json: hand
// ordered top-level keys, several hook events, and a command containing shell
// operators that must not be HTML-escaped on the way back out.
const settingsFixture = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo start"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "rtk hook claude"
          }
        ]
      },
      {
        "matcher": "Artifact",
        "hooks": [
          {
            "type": "command",
            "command": "[ -n \"$X\" ] && \"$X/guard.sh\" || true"
          }
        ]
      }
    ]
  },
  "enabledPlugins": {
    "other@mkt": true
  }
}
`

func mustJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b)
	}
	return v
}

func TestMergeHooksAddsWhilePreservingTheRest(t *testing.T) {
	out, changed, err := MergeHooks([]byte(settingsFixture), WhyfailHooks())
	if err != nil {
		t.Fatalf("MergeHooks: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	s := string(out)
	for _, want := range []string{
		"whyfail hook bash",
		"whyfail hook webfetch",
		"rtk hook claude",
		`"echo start"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output is missing %q", want)
		}
	}
	// Shell operators in a command we did not add must survive verbatim.
	if !strings.Contains(s, `&& \"$X/guard.sh\"`) {
		t.Errorf("existing command was re-encoded:\n%s", s)
	}
	// Assembled at run time: the literal escape sequence does not survive
	// being written into a Go source file unmodified.
	escaped := `\` + `u0026`
	if strings.Contains(s, escaped) {
		t.Errorf("output HTML-escaped a hook command:\n%s", s)
	}
	// Top-level key order is the user's, not alphabetical.
	model := strings.Index(s, `"model"`)
	hooks := strings.Index(s, `"hooks"`)
	plugins := strings.Index(s, `"enabledPlugins"`)
	if model > hooks || hooks > plugins {
		t.Errorf("top-level key order changed: model=%d hooks=%d enabledPlugins=%d", model, hooks, plugins)
	}
}

func TestMergeHooksIndentsNewSectionsForTheirParent(t *testing.T) {
	// A settings file without a hooks key: the section is built at column zero,
	// so its own members have to be laid out for where it will land.
	for name, in := range map[string]string{
		"pretty":  "{\n  \"model\": \"opus\"\n}\n",
		"compact": `{"model": "opus"}`,
		"empty":   "{}",
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := MergeHooks([]byte(in), WhyfailHooks())
			if err != nil {
				t.Fatalf("MergeHooks: %v", err)
			}
			s := string(out)
			if !strings.Contains(s, "\n  \"hooks\": {\n    \"PreToolUse\": [\n") {
				t.Errorf("new sections are not indented for their parent:\n%s", s)
			}
			if !strings.HasSuffix(s, "}\n") && name == "pretty" {
				t.Errorf("the document no longer ends with a closing brace and a newline:\n%q", s)
			}
		})
	}
}

func TestMergeHooksIsIdempotent(t *testing.T) {
	once, changed, err := MergeHooks([]byte(settingsFixture), WhyfailHooks())
	if err != nil || !changed {
		t.Fatalf("first merge: changed=%v err=%v", changed, err)
	}
	twice, changed, err := MergeHooks(once, WhyfailHooks())
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if changed {
		t.Error("second merge reported a change")
	}
	if string(twice) != string(once) {
		t.Error("second merge rewrote the file")
	}
}

func TestMergeHooksCreatesMissingSections(t *testing.T) {
	cases := map[string]string{
		"empty object":     `{}`,
		"no hooks key":     `{"model": "opus"}`,
		"empty hooks":      `{"hooks": {}}`,
		"empty PreToolUse": `{"hooks": {"PreToolUse": []}}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed, err := MergeHooks([]byte(in), WhyfailHooks())
			if err != nil {
				t.Fatalf("MergeHooks: %v", err)
			}
			if !changed {
				t.Fatal("changed = false, want true")
			}
			v := mustJSON(t, out).(map[string]any)
			pt := v["hooks"].(map[string]any)["PreToolUse"].([]any)
			if len(pt) != 2 {
				t.Fatalf("PreToolUse has %d entries, want 2:\n%s", len(pt), out)
			}
		})
	}
}

func TestMergeHooksLeavesForeignPathAlone(t *testing.T) {
	in := `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/whyfail hook bash"}]}]}}`
	// Same matcher, same command text: recognised as present.
	out, changed, err := MergeHooks([]byte(in), WhyfailHooks()[:1])
	if err != nil {
		t.Fatalf("MergeHooks: %v", err)
	}
	if changed {
		t.Errorf("a hook with a different absolute path was treated as absent:\n%s", out)
	}
}

func TestMergeHooksRejectsMalformed(t *testing.T) {
	out, changed, err := MergeHooks([]byte(`{not json`), WhyfailHooks())
	if err == nil {
		t.Fatal("expected an error for malformed settings")
	}
	if out != nil || changed {
		t.Error("malformed input should not produce output")
	}
}

func TestRemoveHooksRestoresTheOriginal(t *testing.T) {
	merged, _, err := MergeHooks([]byte(settingsFixture), WhyfailHooks())
	if err != nil {
		t.Fatalf("MergeHooks: %v", err)
	}
	back, changed, err := RemoveHooks(merged, IsWhyfailHook)
	if err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	if !changed {
		t.Fatal("RemoveHooks reported no change")
	}
	if !reflect.DeepEqual(mustJSON(t, back), mustJSON(t, []byte(settingsFixture))) {
		t.Errorf("round trip did not restore the file:\n%s", back)
	}
}

func TestRemoveHooksPrunesEmptySections(t *testing.T) {
	merged, _, err := MergeHooks([]byte(`{"model": "opus"}`), WhyfailHooks())
	if err != nil {
		t.Fatalf("MergeHooks: %v", err)
	}
	back, changed, err := RemoveHooks(merged, IsWhyfailHook)
	if err != nil || !changed {
		t.Fatalf("RemoveHooks: changed=%v err=%v", changed, err)
	}
	want := map[string]any{"model": "opus"}
	if got := mustJSON(t, back); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRemoveHooksWithoutWhyfailEntries(t *testing.T) {
	back, changed, err := RemoveHooks([]byte(settingsFixture), IsWhyfailHook)
	if err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	if changed {
		t.Error("changed = true for a file with no whyfail hooks")
	}
	if string(back) != settingsFixture {
		t.Error("unchanged file was rewritten")
	}
}

func TestWhyfailHooksMatchThePluginManifest(t *testing.T) {
	want := []HookEntry{
		{Matcher: "Bash", Hooks: []HookCmd{{Type: "command", Command: "whyfail hook bash"}}},
		{Matcher: "WebFetch", Hooks: []HookCmd{{Type: "command", Command: "whyfail hook webfetch"}}},
	}
	if got := WhyfailHooks(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

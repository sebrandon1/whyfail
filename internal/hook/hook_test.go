package hook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, tool, payload string) Output {
	t.Helper()
	var out bytes.Buffer
	if err := Handle(tool, strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	var o Output
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("bad output %q: %v", out.String(), err)
	}
	return o
}

func TestBashRewritesProwFetch(t *testing.T) {
	url := "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/o_r/1/job/2123"
	o := run(t, "bash", `{"tool_name":"Bash","tool_input":{"command":"curl `+url+`"}}`)
	if o.HookSpecificOutput == nil || o.HookSpecificOutput.UpdatedInput["command"] != "whyfail run '"+url+"'" {
		t.Fatalf("got %+v", o)
	}
}

func TestBashIgnoresPipelines(t *testing.T) {
	o := run(t, "bash", `{"tool_name":"Bash","tool_input":{"command":"curl https://prow.ci.openshift.org/view/gs/b/p/1/2 | jq ."}}`)
	if o.HookSpecificOutput != nil {
		t.Fatalf("expected no rewrite, got %+v", o)
	}
}

func TestBashIgnoresUnrelatedCommands(t *testing.T) {
	o := run(t, "bash", `{"tool_name":"Bash","tool_input":{"command":"git status"}}`)
	if o.HookSpecificOutput != nil {
		t.Fatalf("expected no rewrite, got %+v", o)
	}
}

func TestBashDoesNotLoop(t *testing.T) {
	o := run(t, "bash", `{"tool_name":"Bash","tool_input":{"command":"whyfail run https://prow.ci.openshift.org/view/gs/b/p/1/2"}}`)
	if o.HookSpecificOutput != nil {
		t.Fatalf("expected no rewrite, got %+v", o)
	}
}

func TestWebFetchDeniedForProw(t *testing.T) {
	o := run(t, "webfetch", `{"tool_name":"WebFetch","tool_input":{"url":"https://prow.ci.openshift.org/view/gs/b/p/1/2"}}`)
	if o.HookSpecificOutput == nil || o.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("got %+v", o)
	}
	if !strings.Contains(o.HookSpecificOutput.PermissionDecisionReason, "whyfail run") {
		t.Fatalf("got %q", o.HookSpecificOutput.PermissionDecisionReason)
	}
}

func TestWebFetchAllowsOther(t *testing.T) {
	o := run(t, "webfetch", `{"tool_name":"WebFetch","tool_input":{"url":"https://example.com"}}`)
	if o.HookSpecificOutput != nil {
		t.Fatalf("expected allow, got %+v", o)
	}
}

func TestBashRewritesSearchCI(t *testing.T) {
	o := run(t, "bash", `{"tool_name":"Bash","tool_input":{"command":"curl 'https://search.ci.openshift.org/search?search=foo+bar'"}}`)
	if o.HookSpecificOutput == nil {
		t.Fatal("expected rewrite")
	}
	if got := o.HookSpecificOutput.UpdatedInput["command"]; got != "whyfail search 'foo bar'" {
		t.Fatalf("got %v", got)
	}
}

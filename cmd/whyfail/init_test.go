package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/claudecfg"
)

// fakeExec reports claude on PATH, has no plugins installed, and records every
// Run so a test can prove a dry run stayed dry.
type fakeExec struct {
	run []string
}

func (f *fakeExec) deps() claudecfg.Exec {
	return claudecfg.Exec{
		Look: func(string) (string, error) { return "/usr/bin/claude", nil },
		Out:  func(...string) ([]byte, error) { return nil, errors.New("no plugins") },
		Run: func(args ...string) error {
			f.run = append(f.run, strings.Join(args, " "))
			return nil
		},
	}
}

func initTestDeps(t *testing.T, e *fakeExec, out *bytes.Buffer) initDeps {
	t.Helper()
	dir := t.TempDir()
	return initDeps{
		userHome: func() (string, error) { return dir, nil },
		getwd:    func() (string, error) { return dir, nil },
		exec:     e.deps(),
		fs:       fstest.MapFS{},
		binary:   func() string { return "whyfail found at /usr/bin/whyfail" },
		stdout:   out,
	}
}

func TestRunInitFlagErrors(t *testing.T) {
	cases := []struct {
		name string
		opts initOptions
		want string
	}{
		{"both scopes", initOptions{global: true, project: true, from: "x"}, "--global"},
		{"empty from", initOptions{from: "  "}, "--from"},
	}
	for _, c := range cases {
		e := &fakeExec{}
		var out bytes.Buffer
		err := runInit(&globals{}, c.opts, initTestDeps(t, e, &out))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want an error naming %q", c.name, err, c.want)
		}
		if len(e.run) != 0 {
			t.Errorf("%s: ran %v", c.name, e.run)
		}
	}
}

func TestRunInitPathErrors(t *testing.T) {
	for _, which := range []string{"home", "cwd"} {
		e := &fakeExec{}
		var out bytes.Buffer
		deps := initTestDeps(t, e, &out)
		if which == "home" {
			deps.userHome = func() (string, error) { return "", errors.New("no home") }
		} else {
			deps.getwd = func() (string, error) { return "", errors.New("no cwd") }
		}
		err := runInit(&globals{}, initOptions{from: "x"}, deps)
		if err == nil || !strings.Contains(err.Error(), "no "+which) {
			t.Errorf("%s lookup: err = %v, want it reported", which, err)
		}
		if len(e.run) != 0 {
			t.Errorf("%s lookup: installer ran despite the error", which)
		}
	}
}

func TestRunInitJSONDryRun(t *testing.T) {
	e := &fakeExec{}
	var out bytes.Buffer
	if err := runInit(&globals{jsonOut: true}, initOptions{from: "sebrandon1/whyfail", dryRun: true}, initTestDeps(t, e, &out)); err != nil {
		t.Fatal(err)
	}
	var doc initOut
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if doc.Schema != api.SchemaV1 || doc.Mode != "plugin" || !doc.DryRun || !doc.OK {
		t.Errorf("document = %+v", doc)
	}
	if doc.Scope != string(claudecfg.ScopeUser) {
		t.Errorf("scope = %q", doc.Scope)
	}
	planned := false
	for _, s := range doc.Steps {
		if s.Name == "plugin" && s.Status == claudecfg.StatusPlanned {
			planned = true
		}
	}
	if !planned {
		t.Errorf("no planned plugin step: %+v", doc.Steps)
	}
	if len(e.run) != 0 {
		t.Errorf("dry run executed %v", e.run)
	}
}

func TestRunInitHooksOnlyMode(t *testing.T) {
	e := &fakeExec{}
	var out bytes.Buffer
	if err := runInit(&globals{jsonOut: true}, initOptions{from: "x", hooksOnly: true, dryRun: true, project: true}, initTestDeps(t, e, &out)); err != nil {
		t.Fatal(err)
	}
	var doc initOut
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if doc.Mode != "hooks-only" {
		t.Errorf("mode = %q, want hooks-only", doc.Mode)
	}
	if doc.Scope != string(claudecfg.ScopeProject) {
		t.Errorf("scope = %q", doc.Scope)
	}
}

func TestRunInitUninstallHuman(t *testing.T) {
	e := &fakeExec{}
	var out bytes.Buffer
	if err := runInit(&globals{}, initOptions{from: "x", uninstall: true, dryRun: true}, initTestDeps(t, e, &out)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "uninstall") {
		t.Errorf("human output does not name uninstall: %q", out.String())
	}
	if len(e.run) != 0 {
		t.Errorf("dry-run uninstall executed %v", e.run)
	}
}

func TestRunInitProjectNeedsCwd(t *testing.T) {
	e := &fakeExec{}
	var out bytes.Buffer
	deps := initTestDeps(t, e, &out)
	deps.getwd = func() (string, error) { return "", nil }
	err := runInit(&globals{}, initOptions{from: "x", project: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Errorf("err = %v, want a working-directory error", err)
	}
}

func TestRunInitJSONReportsFailure(t *testing.T) {
	e := &fakeExec{}
	var out bytes.Buffer
	deps := initTestDeps(t, e, &out)
	deps.exec.Look = func(string) (string, error) { return "", errors.New("claude missing") }

	err := runInit(&globals{jsonOut: true}, initOptions{from: "x", dryRun: true}, deps)
	if err == nil {
		t.Fatal("want the install failure")
	}
	var doc initOut
	if uerr := json.Unmarshal(out.Bytes(), &doc); uerr != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", uerr, out.String())
	}
	if doc.OK {
		t.Error("ok must be false when the install failed")
	}
}

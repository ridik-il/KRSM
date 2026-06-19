package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scenarioDir(name string) string {
	return filepath.Join("..", "..", "closure", "testdata", "scenarios", name)
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errOut bytes.Buffer
		if err := run([]string{arg}, &out, &errOut); err != nil {
			t.Fatalf("run(%q) returned error: %v", arg, err)
		}
		if got := strings.TrimSpace(out.String()); got != version {
			t.Errorf("run(%q) = %q, want %q", arg, got, version)
		}
	}
}

func TestRunHelpIsDefault(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut); err != nil {
		t.Fatalf("run(nil) returned error: %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("default output missing usage banner; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "check") {
		t.Errorf("usage banner does not list the check command; got:\n%s", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"bogus"}, &out, &errOut); err == nil {
		t.Fatal("run(bogus) = nil error, want an error for an unknown command")
	}
}

// TestCheckBlock: a scenario whose closure escapes scope prints BLOCK with the
// escaping refs on stdout and signals the block via the errBlocked sentinel.
func TestCheckBlock(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", scenarioDir("01-memory-pressure-cascade")}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("run(check, block scenario) err = %v, want errBlocked", err)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "BLOCK") {
		t.Errorf("stdout missing BLOCK verdict; got:\n%s", stdout)
	}
	for _, ref := range []string{"Pod/prod/web-2", "Pod/prod/web-3"} {
		if !strings.Contains(stdout, ref) {
			t.Errorf("stdout missing escaping ref %q; got:\n%s", ref, stdout)
		}
	}
}

// writeAllowScenario writes a minimal in-scope scenario (delete a Pod that is the
// sole closure member and is itself authorised) and returns its directory.
func writeAllowScenario(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"cluster.yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: web-1\n  namespace: prod\n",
		"request.yaml": "verb: delete\ntarget:\n  kind: Pod\n  namespace: prod\n  name: web-1\n",
		"scope.yaml":   "scope:\n  - kind: Pod\n    namespace: prod\n    name: web-1\n",
	}
	for f, c := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return dir
}

// TestCheckAllow: an in-scope action prints ALLOW on stdout, returns nil, no detail.
func TestCheckAllow(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"check", writeAllowScenario(t)}, &out, &errOut); err != nil {
		t.Fatalf("run(check, allow scenario) = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "ALLOW") {
		t.Errorf("stdout missing ALLOW verdict; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "→") {
		t.Errorf("ALLOW should have no detail lines; got:\n%s", out.String())
	}
}

// TestCheckWarnGoesToStderr: a WARN verdict and its external detail are written to
// stderr (exit 0), while stdout carries only the factual ACTION/SCOPE/CLOSURE.
func TestCheckWarnGoesToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"check", scenarioDir("09-finalizer-orphans-external")}, &out, &errOut); err != nil {
		t.Fatalf("run(check, warn scenario) = %v, want nil", err)
	}
	if !strings.Contains(errOut.String(), "WARN") {
		t.Errorf("stderr missing WARN verdict; got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "External/prod/example.com/external-lb") {
		t.Errorf("stderr missing external ref; got:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "WARN") {
		t.Errorf("stdout must not carry the WARN verdict; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "CLOSURE") {
		t.Errorf("stdout should still carry the factual report; got:\n%s", out.String())
	}
}

// TestCheckFailClosed: an unknown target denies fail-closed (errBlocked) with a
// reason that names the fail-closed condition.
func TestCheckFailClosed(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", scenarioDir("11-unknown-target-fail-closed")}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("run(check, fail-closed scenario) err = %v, want errBlocked", err)
	}
	if !strings.Contains(out.String(), "fail-closed") {
		t.Errorf("stdout missing fail-closed reason; got:\n%s", out.String())
	}
}

// TestCheckMissingDirIsUsageError: no directory argument is a usage error (exit 1),
// distinct from a Block (errBlocked / exit 2).
func TestCheckMissingDirIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check"}, &out, &errOut)
	if err == nil {
		t.Fatal("run(check) with no dir = nil error, want a usage error")
	}
	if errors.Is(err, errBlocked) {
		t.Error("missing-dir error must not be errBlocked (that would imply exit 2, not 1)")
	}
}

// TestCheckReportShape: the report shows ACTION (verb + target), SCOPE, and CLOSURE.
func TestCheckReportShape(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"check", scenarioDir("01-memory-pressure-cascade")}, &out, &errOut)
	stdout := out.String()
	for _, want := range []string{"ACTION", "delete Deployment/prod/web", "SCOPE", "CLOSURE"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q; got:\n%s", want, stdout)
		}
	}
}

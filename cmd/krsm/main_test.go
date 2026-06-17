package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out bytes.Buffer
		if err := run([]string{arg}, &out); err != nil {
			t.Fatalf("run(%q) returned error: %v", arg, err)
		}
		if got := strings.TrimSpace(out.String()); got != version {
			t.Errorf("run(%q) = %q, want %q", arg, got, version)
		}
	}
}

func TestRunHelpIsDefault(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("run(nil) returned error: %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("default output missing usage banner; got:\n%s", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"bogus"}, &out); err == nil {
		t.Fatal("run(bogus) = nil error, want an error for an unknown command")
	}
}

// Command krsm is the entrypoint for the KRSM scope gate.
//
// KRSM decides, before a Kubernetes action reaches the API server, whether the
// action's affected-resource closure over live cluster state stays within the
// task's authorised scope. This binary is in early development; see
// docs/ROADMAP.md for what is implemented today.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/scenario"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// errBlocked signals that a `check` produced a Block verdict. It carries status,
// not a user-facing message — the block report is already on stdout — so main
// maps it to a distinct exit code (2) without printing an error line.
var errBlocked = errors.New("blocked")

const usage = `krsm — a pre-execution safety gate for autonomous Kubernetes agents.

Usage:
  krsm <command>

Commands:
  check <dir>   Run the closure check for a scenario directory
                (cluster.yaml, request.yaml, scope.yaml) and print the verdict
  version       Print the krsm version
  help          Show this help

Exit codes for check: 0 allow/warn, 2 block, 1 usage or load error.

KRSM is in early development. See docs/ROADMAP.md for the build plan
and docs/DESIGN.md for the architecture.
`

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
		// exit 0
	case errors.Is(err, errBlocked):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "krsm:", err)
		os.Exit(1)
	}
}

// run dispatches a single command. User-facing output goes to stdout; warnings
// and diagnostics go to stderr, so both can be tested without touching the
// process streams. A Block verdict is returned as errBlocked (see main).
func run(args []string, stdout, stderr io.Writer) error {
	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q (try \"krsm help\")", cmd)
	}
	return nil
}

// runCheck loads a scenario directory, runs closure.Safe, and writes the report.
func runCheck(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("check: missing scenario directory (usage: krsm check <dir>)")
	}
	sc, err := scenario.Load(args[0])
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	dec := closure.Safe(sc.State, sc.Action, sc.Scope)
	return writeReport(stdout, stderr, sc, dec)
}

// writeReport prints the ACTION/SCOPE/CLOSURE report (always to stdout) and the
// verdict, routed by severity: ALLOW and BLOCK to stdout, WARN to stderr. It
// returns errBlocked on a Block verdict so the caller can set the exit code.
func writeReport(stdout, stderr io.Writer, sc *scenario.Scenario, dec closure.Decision) error {
	fmt.Fprintf(stdout, "%-8s %s %s\n", "ACTION", sc.Action.Verb, sc.Action.Target)
	fmt.Fprintf(stdout, "%-8s %s\n", "SCOPE", joinScope(sc.Scope))
	fmt.Fprintf(stdout, "%-8s %s\n", "CLOSURE", joinRefs(dec.Closure))

	switch dec.Verdict {
	case closure.Block:
		fmt.Fprintf(stdout, "%-8s ❌ BLOCK — %s:\n", "VERDICT", dec.Reason)
		writeDetail(stdout, dec.Escaping)
		return errBlocked
	case closure.Warn:
		fmt.Fprintf(stderr, "%-8s ⚠ WARN — %s:\n", "VERDICT", dec.Reason)
		writeDetail(stderr, dec.External)
		return nil
	default:
		fmt.Fprintf(stdout, "%-8s ✅ ALLOW — closure within task scope\n", "VERDICT")
		return nil
	}
}

func writeDetail(w io.Writer, refs []closure.Ref) {
	for _, r := range refs {
		fmt.Fprintf(w, "           → %s\n", r)
	}
}

func joinRefs(refs []closure.Ref) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

func joinScope(scope []closure.ScopeRef) string {
	parts := make([]string, len(scope))
	for i, s := range scope {
		parts[i] = fmt.Sprintf("%s/%s/%s", s.GVK.Kind, s.Namespace, s.Name)
	}
	return strings.Join(parts, ", ")
}

// Command krsm is the entrypoint for the KRSM scope gate.
//
// KRSM decides, before a Kubernetes action reaches the API server, whether the
// action's affected-resource closure over live cluster state stays within the
// task's authorised scope. This binary is in early development; see
// docs/ROADMAP.md for what is implemented today.
package main

import (
	"errors"
	"flag"
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
  check [--plain] <dir>   Run the closure check for a scenario directory
                          (cluster.yaml, request.yaml, scope.yaml) and print
                          the verdict. --plain emits ASCII without emoji.
  version                 Print the krsm version
  help                    Show this help

Exit codes for check: 0 allow/warn, 2 block, 1 usage or load error.

KRSM is in early development. See docs/ROADMAP.md for the build plan
and docs/DESIGN.md for the architecture.
`

const checkUsage = `Usage: krsm check [--plain] <scenario-dir>

Loads cluster.yaml, request.yaml and scope.yaml from <scenario-dir>, computes the
affected-resource closure, and prints the ACTION / SCOPE / CLOSURE / VERDICT report.

Flags:
  --plain   ASCII output without emoji (for CI logs / non-UTF8 terminals)

Exit codes: 0 allow/warn, 2 block, 1 usage or load error.
A WARN's cross-boundary detail is written to stderr.
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

// runCheck parses the check flags, loads the scenario, runs closure.Safe, writes
// the report, and derives the exit status: a Block verdict becomes errBlocked.
// Keeping the verdict→status decision here (not in the formatter) separates
// presentation from control flow.
func runCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we render usage and flag errors ourselves
	plain := fs.Bool("plain", false, "ASCII output without emoji")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, checkUsage)
			return nil
		}
		return fmt.Errorf("check: %w", err)
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("check: missing scenario directory (usage: krsm check [--plain] <dir>)")
	}
	// Go's flag package stops at the first positional, so a flag placed after the
	// dir (e.g. `check <dir> --plain`) would be silently dropped. Reject any extra
	// positional rather than run with a misread invocation.
	if fs.NArg() > 1 {
		return fmt.Errorf("check: unexpected argument %q; flags must precede <dir> (usage: krsm check [--plain] <dir>)", fs.Arg(1))
	}

	sc, err := scenario.Load(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	dec := closure.Safe(sc.State, sc.Action, sc.Scope)
	writeReport(stdout, stderr, sc, dec, *plain)
	if dec.Verdict == closure.Block {
		return errBlocked
	}
	return nil
}

// writeReport renders the report. The factual ACTION/SCOPE/CLOSURE lines and the
// ALLOW/BLOCK verdict go to stdout; a WARN writes a self-contained verdict stub
// to stdout and its cross-boundary detail to stderr. It returns nothing — the
// caller owns the exit-status decision.
func writeReport(stdout, stderr io.Writer, sc *scenario.Scenario, dec closure.Decision, plain bool) {
	fmt.Fprintf(stdout, "%-8s %s %s\n", "ACTION", sc.Action.Verb, sc.Action.Target)
	fmt.Fprintf(stdout, "%-8s %s\n", "SCOPE", joinScope(sc.Scope))
	fmt.Fprintf(stdout, "%-8s %s\n", "CLOSURE", joinRefs(dec.Closure))

	switch dec.Verdict {
	case closure.Block:
		fmt.Fprintf(stdout, "%-8s %sBLOCK — %s:\n", "VERDICT", icon(closure.Block, plain), dec.Reason)
		writeDetail(stdout, dec.Escaping)
	case closure.Warn:
		fmt.Fprintf(stdout, "%-8s %sWARN — %s (detail on stderr)\n", "VERDICT", icon(closure.Warn, plain), dec.Reason)
		fmt.Fprintf(stderr, "%-8s %sWARN — %s:\n", "VERDICT", icon(closure.Warn, plain), dec.Reason)
		writeDetail(stderr, dec.External)
	default:
		fmt.Fprintf(stdout, "%-8s %sALLOW — closure within task scope\n", "VERDICT", icon(closure.Allow, plain))
	}
}

// icon returns the verdict marker. In plain mode it is empty, so the verdict word
// stands alone (no width-ambiguous emoji to misalign CI logs / non-UTF8 terminals).
func icon(v closure.Verdict, plain bool) string {
	if plain {
		return ""
	}
	switch v {
	case closure.Block:
		return "❌ "
	case closure.Warn:
		return "⚠ "
	default:
		return "✅ "
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
		parts[i] = scopeStr(s)
	}
	return strings.Join(parts, ", ")
}

// scopeStr renders a scope clause as Kind/namespace/name, qualifying the kind with
// its API group (Kubernetes "Kind.group" form) when set, so two clauses that
// differ only by group are unambiguous.
func scopeStr(s closure.ScopeRef) string {
	kind := s.GVK.Kind
	if s.GVK.Group != "" {
		kind += "." + s.GVK.Group
	}
	return fmt.Sprintf("%s/%s/%s", kind, s.Namespace, s.Name)
}

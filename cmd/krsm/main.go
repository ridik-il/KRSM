// Command krsm is the entrypoint for the KRSM scope gate.
//
// KRSM decides, before a Kubernetes action reaches the API server, whether the
// action's affected-resource closure over live cluster state stays within the
// task's authorised scope. This binary is in early development; see
// docs/ROADMAP.md for what is implemented today.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `krsm — a pre-execution safety gate for autonomous Kubernetes agents.

Usage:
  krsm <command>

Commands:
  version    Print the krsm version
  help       Show this help

KRSM is in early development. See docs/ROADMAP.md for the build plan
and docs/DESIGN.md for the architecture.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "krsm:", err)
		os.Exit(1)
	}
}

// run dispatches a single command, writing user-facing output to out so it can
// be tested without touching the process streams.
func run(args []string, out io.Writer) error {
	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintln(out, version)
	case "help", "--help", "-h":
		fmt.Fprint(out, usage)
	default:
		return fmt.Errorf("unknown command %q (try \"krsm help\")", cmd)
	}
	return nil
}

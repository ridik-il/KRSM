// Command krsm is the entrypoint for the KRSM scope gate.
//
// KRSM decides, before a Kubernetes action reaches the API server, whether the
// action's affected-resource closure over live cluster state stays within the
// task's authorised scope. This binary is in early development; see
// docs/ROADMAP.md for what is implemented today.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
	"github.com/ridik-il/krsm/internal/scenario"
	"github.com/ridik-il/krsm/scope"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// liveReader is the live-cluster seam the CLI consumes: it resolves a target's
// canonical Kind and reads the cluster into a closure.State, both READ-ONLY and
// fail-closed (an error, never a shrunk State). *cluster.Reader satisfies it; tests
// substitute a fake so the live path runs with no cluster.
type liveReader interface {
	State(ctx context.Context) (closure.State, error)
	ResolveKind(ctx context.Context, kind, group string) (string, error)
}

// newLiveReader resolves a *rest.Config from the kubeconfig/context flags and builds
// a liveReader from it. It is a package variable so tests can swap in a hermetic fake
// (bypassing kubeconfig resolution entirely); production resolves the config and
// builds a *cluster.Reader. The config carries the bearer token / client cert and is
// never logged — it is handed straight to NewReader.
var newLiveReader = func(kubeconfig, contextName string) (liveReader, error) {
	cfg, err := restConfig(kubeconfig, contextName)
	if err != nil {
		return nil, err
	}
	return cluster.NewReader(cfg)
}

// errBlocked signals that a `check` produced a Block verdict. It carries status,
// not a user-facing message — the block report is already on stdout — so main
// maps it to a distinct exit code (2) without printing an error line.
var errBlocked = errors.New("blocked")

const usage = `krsm — a pre-execution safety gate for autonomous Kubernetes agents.

Usage:
  krsm <command>

Commands:
  check [flags] <dir>     Run the closure check for a scenario directory and
                          print the verdict. The dir needs cluster.yaml +
                          request.yaml; scope is optional — it comes from
                          taskcontract.yaml OR scope.yaml, and is otherwise
                          derived (a Level-0 ownership tree of the target).
                          With --context/--kubeconfig instead runs against a LIVE
                          cluster (read-only): krsm check --context X <verb>
                          <Kind/name> [-n ns]. See "krsm check --help".
                          --plain emits ASCII without emoji; --mode
                          audit|enforce governs scope-escape handling.
  version                 Print the krsm version
  help                    Show this help

Exit codes for check: 0 allow/warn, 2 block, 1 usage or load error.

KRSM is in early development. See docs/ROADMAP.md for the build plan
and docs/DESIGN.md for the architecture.
`

const checkUsage = `Usage:
  krsm check [--plain] [--mode audit|enforce] <scenario-dir>
  krsm check [--context X] [--kubeconfig P] [--plain] [--mode audit|enforce] [--timeout 30s] <verb> <Kind/name> [-n ns]

Scenario-dir path: requires cluster.yaml + request.yaml from <scenario-dir>. Scope is
optional — it comes from taskcontract.yaml OR scope.yaml, otherwise derived (a Level-0
ownership tree of the target).

Live-cluster path: selected when --context and/or --kubeconfig is given. Reads the live
cluster READ-ONLY, parses "<verb> <Kind/name> [-n ns]" as the target action, and derives
the Level-0 ownership scope (no contract on the CLI). <Kind> may be the Kind, the
lowercased kind, or the resource name (deployment/deployments) — it is normalized to the
canonical Kind via API discovery. Supported verbs: delete (cascade), scale, restart;
mutation verbs (update/patch) are deferred (they need a request payload with no CLI form).

Both paths compute the affected-resource closure and print the
ACTION / SCOPE / CLOSURE / VERDICT report.

Flags:
  --context string     kubeconfig context; selects the live-cluster path
  --kubeconfig string  kubeconfig path; selects the live-cluster path (else KUBECONFIG /
                       ~/.kube/config)
  -n string            target namespace (live path)
  --plain              ASCII output without emoji (for CI logs / non-UTF8 terminals)
  --mode string        audit (default) downgrades a scope-escape Block to Warn so a
                       day-0 false positive does not deny; enforce keeps the Block.
  --timeout duration   live-path deadline (default 30s); cancels on SIGINT/SIGTERM. 0
                       disables the deadline (signal-cancel only). A deadline/cancel is a
                       fail-closed cluster-read error (exit 1), never a partial snapshot.

Exit codes: 0 allow/warn, 2 block, 1 usage / load / cluster-read error.
A cluster-read failure (forbidden/unreadable kind or discovery failure) is a fail-closed
deny: exit 1 with a "krsm: check: …" reason, never a partial snapshot.
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
	modeStr := fs.String("mode", string(scope.ModeAudit), "scope-escape handling: audit|enforce")
	ctxFlag := fs.String("context", "", "kubeconfig context (selects the live-cluster path)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (selects the live-cluster path)")
	namespace := fs.String("n", "", "target namespace (live path)")
	timeout := fs.Duration("timeout", 30*time.Second, "live-path deadline (e.g. 30s); 0 disables the deadline (signal-cancel only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, checkUsage)
			return nil
		}
		return fmt.Errorf("check: %w", err)
	}
	mode, err := parseMode(*modeStr)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}

	// Path selection: a cluster flag (--context and/or --kubeconfig) selects the LIVE
	// path; otherwise a single positional directory keeps the existing scenario path
	// (unchanged). The two paths share Safe/mode/writeReport — only how they build the
	// State, Action and scope differs.
	if *ctxFlag != "" || *kubeconfig != "" {
		return runCheckLive(fs.Args(), liveOpts{
			contextName: *ctxFlag,
			kubeconfig:  *kubeconfig,
			namespace:   *namespace,
			mode:        mode,
			plain:       *plain,
			timeout:     *timeout,
		}, stdout, stderr)
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("check: missing scenario directory (usage: krsm check [--plain] [--mode audit|enforce] <dir>)")
	}
	// Go's flag package stops at the first positional, so a flag placed after the
	// dir (e.g. `check <dir> --plain`) would be silently dropped. Reject any extra
	// positional rather than run with a misread invocation.
	if fs.NArg() > 1 {
		return fmt.Errorf("check: unexpected argument %q; flags must precede <dir> (usage: krsm check [--plain] [--mode audit|enforce] <dir>)", fs.Arg(1))
	}

	sc, err := scenario.Load(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	// Apply the mode above the unchanged closure.Safe: in audit a scope-escape Block
	// is downgraded to Warn (DESIGN §"Mode application"); the exit-status decision
	// below acts on the applied verdict.
	dec := mode.Apply(closure.Safe(sc.State, sc.Action, sc.Scope))
	writeReport(stdout, stderr, sc.Action, sc.Scope, sc.ScopeSource, dec, *plain)
	if dec.Verdict == closure.Block {
		return errBlocked
	}
	return nil
}

// liveOpts carries the resolved check flags into the live path.
type liveOpts struct {
	contextName string
	kubeconfig  string
	namespace   string
	mode        scope.Mode
	plain       bool
	timeout     time.Duration
}

// runCheckLive runs a check against a live cluster. It resolves a *rest.Config from
// standard client-go rules (KUBECONFIG / ~/.kube/config) with the --kubeconfig and
// --context overrides, reads the cluster READ-ONLY into a closure.State, parses the
// "<verb> <Kind/name>" target (normalizing <Kind> to the canonical Kind via discovery
// so the uid-less target resolves by its human key), DERIVES the Level-0 ownership
// scope (no contract on the CLI), and runs the SHARED Safe/mode/writeReport.
//
// A cluster-read failure (discovery or list error — slice 4) is an operational error:
// it returns a "check: …" error (exit 1) with a fail-closed reason, never a partial
// snapshot. An unresolvable target is the engine's existing fail-closed Block (exit 2).
//
// [assumed: a cluster-read failure exits 1, matching the existing default error path
// (main maps a non-errBlocked error to exit 1); the plan only requires "non-zero with
// a fail-closed reason, not a smaller closure".]
func runCheckLive(args []string, o liveOpts, stdout, stderr io.Writer) error {
	// Go's flag package stops at the first positional, so a `-n ns` placed AFTER the
	// verb/target (the documented kubectl-style form) lands here unparsed. Extract it
	// from the positionals so the namespace can follow the target.
	positional, ns, err := extractNamespace(args)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if ns != "" {
		o.namespace = ns
	}
	if len(positional) < 2 {
		return fmt.Errorf("check: live path needs a verb and a target (usage: krsm check [--context X] [--kubeconfig P] [--mode audit|enforce] <verb> <Kind/name> [-n ns])")
	}
	if len(positional) > 2 {
		return fmt.Errorf("check: unexpected argument %q (usage: krsm check [--context X] [--kubeconfig P] <verb> <Kind/name> [-n ns])", positional[2])
	}
	verb, target := positional[0], positional[1]

	reader, err := newLiveReader(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}

	// Bound the live read (S1): cancel on SIGINT/SIGTERM and, unless --timeout 0, after the
	// deadline. A deadline/cancel surfaces through ResolveKind/State as a read error → the
	// existing fail-closed path below; never a closure over a partial snapshot.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	// Split the target ONCE here; downstream takes the pieces, never the "Kind/name"
	// string again (no duplicate re-validation).
	kindTok, group, name, err := splitTarget(target)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	// Normalize the user's <Kind>[.<group>] token to the canonical Kind discovery reports, so
	// the uid-less target Ref carries the Kind the live State indexes by (Ref.human); the
	// optional group disambiguates a Kind served by several API groups (fail-closed if
	// ambiguous and unqualified).
	canonicalKind, err := reader.ResolveKind(ctx, kindTok, group)
	if err != nil {
		// Fail closed: a kind the cluster does not report (or a discovery failure) is an
		// operational deny, never a guessed Kind that resolves no target.
		return fmt.Errorf("check: %w", err)
	}

	action, err := parseAction(verb, canonicalKind, name, o.namespace)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}

	state, err := reader.State(ctx)
	if err != nil {
		// Fail closed (slice 4): a discovery/list error is an UNKNOWN closure, denied as
		// an operational error rather than evaluated over a partial snapshot.
		return fmt.Errorf("check: cluster read failed, denying fail-closed: %w", err)
	}

	// No contract/annotation on the CLI: derive the Level-0 ownership scope of the target.
	pred := scope.Derive(action.Target)
	dec := o.mode.Apply(closure.Safe(state, action, pred.Clauses))
	writeReport(stdout, stderr, action, pred.Clauses, scopeSourceDerived, dec, o.plain)
	if dec.Verdict == closure.Block {
		return errBlocked
	}
	return nil
}

// extractNamespace pulls a kubectl-style `-n <ns>` / `-n=<ns>` / `--namespace <ns>`
// out of the positional args (where Go's flag package leaves it when it follows the
// verb/target), returning the remaining positionals and the namespace. A `-n` with no
// value is a usage error. It tolerates the namespace flag appearing anywhere in the
// trailing positionals so `<verb> <Kind/name> -n ns` parses.
//
// It HARDENS against a misplaced target: a namespace value containing "/" (e.g.
// `-n Deployment/web`, where the user put the target after -n) is rejected rather than
// silently consumed as the namespace — which would otherwise run the check against a
// nonsensical namespace and likely fail-close with a confusing reason. A Kubernetes
// namespace name is a DNS label and never contains a slash, so this rejects only
// mistakes.
func extractNamespace(args []string) (positional []string, namespace string, err error) {
	take := func(flag, val string) (string, error) {
		if strings.Contains(val, "/") {
			return "", fmt.Errorf("namespace %q looks like a Kind/name target; %s takes a namespace, put the target before it (e.g. delete Deployment/web -n prod)", val, flag)
		}
		return val, nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--namespace":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("flag %s needs a namespace value", a)
			}
			if namespace, err = take(a, args[i+1]); err != nil {
				return nil, "", err
			}
			i++
		case strings.HasPrefix(a, "-n="):
			if namespace, err = take("-n", strings.TrimPrefix(a, "-n=")); err != nil {
				return nil, "", err
			}
		case strings.HasPrefix(a, "--namespace="):
			if namespace, err = take("--namespace", strings.TrimPrefix(a, "--namespace=")); err != nil {
				return nil, "", err
			}
		default:
			positional = append(positional, a)
		}
	}
	return positional, namespace, nil
}

// scopeSourceDerived is the operator-facing provenance for a CLI-derived scope,
// matching internal/scenario's scopeSourceDerived so both paths print one phrase.
const scopeSourceDerived = "derived (ownership-tree)"

// restConfig resolves a *rest.Config from standard client-go loading rules
// (KUBECONFIG, then ~/.kube/config), with --kubeconfig overriding the path and
// --context overriding the current context. The resolved config carries the bearer
// token / client cert and is NEVER logged — it is handed straight to the reader.
func restConfig(kubeconfig, contextName string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve kubeconfig: %w", err)
	}
	return cfg, nil
}

// liveVerbs are the verbs the live CLI path supports today. Mutation verbs
// (update/patch) need an old/new request payload that has no CLI form yet
// (open Q4, deferred — docs/plans/v0.4-live-cluster-reads.md decision 3), so the
// live path accepts only the non-mutation verbs: delete (cascade), scale, restart.
// [assumed: a CLI delete defaults to a cascading delete (Cascade=true), mirroring
// scenario.parseAction's `Cascade: ra.Cascade == nil || *ra.Cascade`; scale/restart
// carry no cascade payload and leave Cascade=false.]
var liveVerbs = map[closure.Verb]bool{
	closure.Delete:  true,
	closure.Scale:   true,
	closure.Restart: true,
}

// splitTarget splits a "<Kind>[.<group>]/<name>" target token into its Kind, an optional
// API group, and the name, rejecting a missing slash, an empty kind, or an empty name with
// one clear usage error. The optional ".<group>" qualifier disambiguates a Kind served by
// several API groups (S2 #16): the name is split on the FIRST "/", then the Kind side is
// split on the FIRST "." (so a multi-dot group such as "infra.example.com" is preserved as
// the group). A trailing dot with no group ("Deployment./web") is rejected. It is the SINGLE
// place the target form is validated — runCheckLive calls it once, then hands the pieces
// (after Kind normalization) to parseAction, so the form is never re-split downstream.
func splitTarget(target string) (kind, group, name string, err error) {
	kindGroup, name, ok := strings.Cut(target, "/")
	if !ok || kindGroup == "" || name == "" {
		return "", "", "", fmt.Errorf("invalid target %q (want <Kind>[.<group>]/<name>, e.g. Deployment/web or Deployment.apps/web)", target)
	}
	kind, group, hasDot := strings.Cut(kindGroup, ".")
	if kind == "" || (hasDot && group == "") {
		return "", "", "", fmt.Errorf("invalid target %q (want <Kind>[.<group>]/<name>, e.g. Deployment/web or Deployment.apps/web)", target)
	}
	return kind, group, name, nil
}

// parseAction builds a closure.Action from a verb and an ALREADY-SPLIT canonical Kind,
// name and namespace (runCheckLive does the single split via splitTarget and resolves
// the canonical Kind first). The target Ref carries NO UID — the live State resolves
// the target by its human key (Kind/ns/name) since the CLI has no uid — so kind MUST be
// the canonical Kind (the live object's `kind` field).
//
// Mutation verbs (update/patch) are rejected: their old/new payload has no CLI form
// (open Q4, deferred). An unknown verb is a usage error; kind/name are validated as a
// defensive non-empty check (splitTarget already guarantees it) so a future caller
// cannot build a nameless action.
func parseAction(verb, kind, name, namespace string) (closure.Action, error) {
	v := closure.Verb(verb)
	if !liveVerbs[v] {
		if v == closure.Update || v == closure.Patch {
			return closure.Action{}, fmt.Errorf("verb %q is not yet supported on the live path: a mutation needs an old/new payload with no CLI form (deferred); use a scenario directory for mutation actions", verb)
		}
		return closure.Action{}, fmt.Errorf("unknown verb %q (want one of delete, scale, restart)", verb)
	}
	if kind == "" || name == "" {
		return closure.Action{}, fmt.Errorf("invalid target %q/%q (Kind and name are both required)", kind, name)
	}
	return closure.Action{
		Verb:    v,
		Target:  closure.Ref{GVK: closure.GVK{Kind: kind}, Namespace: namespace, Name: name},
		Cascade: v == closure.Delete,
	}, nil
}

// parseMode validates the --mode value, rejecting anything but audit|enforce with a
// usage error (exit 1) rather than letting an unknown mode fall through to a silent
// pass-through. The default is wired by the flag's default value (ModeAudit).
func parseMode(s string) (scope.Mode, error) {
	switch scope.Mode(s) {
	case scope.ModeAudit, scope.ModeEnforce:
		return scope.Mode(s), nil
	default:
		return "", fmt.Errorf("invalid --mode %q (want %q or %q)", s, scope.ModeAudit, scope.ModeEnforce)
	}
}

// writeReport renders the report. The factual ACTION/SCOPE/CLOSURE lines and the
// ALLOW/BLOCK verdict go to stdout; a WARN writes a self-contained verdict stub
// to stdout and its cross-boundary detail to stderr. It returns nothing — the
// caller owns the exit-status decision.
//
// It takes the three report inputs (the action, the scope clauses, and the human
// scope-source string) rather than a *scenario.Scenario, so the live-cluster path —
// which has no Scenario — shares this writer verbatim with the scenario-dir path.
func writeReport(stdout, stderr io.Writer, action closure.Action, clauses []closure.ScopeClause, scopeSource string, dec closure.Decision, plain bool) {
	fmt.Fprintf(stdout, "%-8s %s %s\n", "ACTION", action.Verb, action.Target)
	fmt.Fprintf(stdout, "%-8s %s  (%s)\n", "SCOPE", joinScope(clauses), scopeSource)
	fmt.Fprintf(stdout, "%-8s %s\n", "CLOSURE", joinRefs(dec.Closure))

	switch {
	case dec.Verdict == closure.Block:
		fmt.Fprintf(stdout, "%-8s %sBLOCK — %s:\n", "VERDICT", icon(closure.Block, plain), dec.Reason)
		writeDetail(stdout, dec.Escaping)
	case dec.Verdict == closure.Warn && len(dec.Escaping) > 0:
		// An audit-downgraded scope-escape Warn (scope.ModeAudit.Apply): the verdict
		// is WARN (exit 0) but the escaping members are what WOULD block under enforce,
		// so they stay on stdout — the operator must see them — and the Reason already
		// marks the audit downgrade.
		fmt.Fprintf(stdout, "%-8s %sWARN — %s:\n", "VERDICT", icon(closure.Warn, plain), dec.Reason)
		writeDetail(stdout, dec.Escaping)
		// closure.Safe populates BOTH Escaping and External on the same Decision, so a
		// downgraded scope-escape may also carry cross-boundary effects. Surface them on
		// stderr with the same labeled header the pure-Warn branch uses, rather than
		// silently dropping the External detail.
		if len(dec.External) > 0 {
			fmt.Fprintf(stderr, "%-8s %sWARN — closure crosses the cluster boundary (external effect):\n", "VERDICT", icon(closure.Warn, plain))
			writeDetail(stderr, dec.External)
		}
	case dec.Verdict == closure.Warn:
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

func joinScope(scope []closure.ScopeClause) string {
	parts := make([]string, len(scope))
	for i, s := range scope {
		parts[i] = scopeStr(s)
	}
	return strings.Join(parts, ", ")
}

// scopeStr renders a scope clause readably for the SCOPE line, one form per
// dimension. The kind is always qualified with its API group (Kubernetes "Kind.group"
// form) when set, so two clauses differing only by group are unambiguous:
//
//   - resource:  Kind[.group]/ns/name      (name may be a glob)
//   - selector:  Kind[.group]/ns/{selector} (selector between braces, e.g. {app In [web]})
//   - namespace: ns:[Kind[.group]/]ns/*     (the whole namespace, GVK an optional gate)
//   - ownership: owns:Kind[.group]/ns/name  (the Root's identity; covers its subtree)
func scopeStr(s closure.ScopeClause) string {
	switch s.Dim {
	case closure.DimNamespace:
		if s.GVK.Kind == "" {
			return fmt.Sprintf("ns:%s/*", s.Namespace)
		}
		return fmt.Sprintf("ns:%s/%s/*", qualifiedKind(s.GVK), s.Namespace)
	case closure.DimOwnership:
		return fmt.Sprintf("owns:%s/%s/%s", qualifiedKind(s.Root.GVK), s.Root.Namespace, s.Root.Name)
	case closure.DimSelector:
		return fmt.Sprintf("%s/%s/{%s}", qualifiedKind(s.GVK), s.Namespace, selectorStr(s.Selector))
	default: // DimResource and the empty (back-compat) dim
		return fmt.Sprintf("%s/%s/%s", qualifiedKind(s.GVK), s.Namespace, s.Name)
	}
}

// qualifiedKind renders a GVK's kind, qualified with its API group as "Kind.group"
// when the group is set (the core group stays bare), so clauses differing only by
// group are distinguishable on the SCOPE line.
func qualifiedKind(gvk closure.GVK) string {
	if gvk.Group == "" {
		return gvk.Kind
	}
	return gvk.Kind + "." + gvk.Group
}

// selectorStr renders a LabelSelector readably for the SCOPE line: matchLabels as
// `key=value` and each matchExpressions requirement as `key Op [values]`, joined by
// commas (the AND the selector evaluates). Used only for human output.
func selectorStr(sel closure.LabelSelector) string {
	parts := make([]string, 0, len(sel.MatchLabels)+len(sel.MatchExpressions))
	keys := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+sel.MatchLabels[k])
	}
	for _, r := range sel.MatchExpressions {
		switch r.Operator {
		case closure.OpExists, closure.OpDoesNotExist:
			parts = append(parts, fmt.Sprintf("%s %s", r.Key, r.Operator))
		default:
			parts = append(parts, fmt.Sprintf("%s %s %v", r.Key, r.Operator, r.Values))
		}
	}
	return strings.Join(parts, ", ")
}

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ridik-il/krsm/closure"
)

func scenarioDir(name string) string {
	return filepath.Join("..", "..", "closure", "testdata", "scenarios", name)
}

// TestCheckSelectorScenarioAllows: the selector-scope proof scenario (20) reports
// ALLOW (exit 0, no errBlocked) and renders the selector clause in the SCOPE line
// between braces (e.g. Pod/prod/{app In [web]}), while the resource clause still
// renders as Kind/ns/name.
func TestCheckSelectorScenarioAllows(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", scenarioDir("20-scope-selector-precision")}, &out, &errOut)
	if err != nil {
		t.Fatalf("run(check, selector scenario) = %v, want nil (ALLOW, exit 0)", err)
	}
	stdout := out.String()
	for _, want := range []string{
		"ALLOW",
		"Deployment/prod/frontend", // resource clause renders unchanged
		"Pod/prod/{app In [web]}",  // selector clause renders readably
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, stdout)
		}
	}
}

// TestWriteReportAuditDowngradeRendersExternal: closure.Safe populates BOTH Escaping
// and External on the same Decision (a scope escape that also crosses the cluster
// boundary). When --mode audit downgrades that Block to Warn, writeReport's
// audit-downgraded branch must render the External refs too — not silently drop the
// cross-boundary detail. The external refs are written somewhere visible (stdout or
// stderr), consistent with how the pure-Warn branch surfaces External detail.
func TestWriteReportAuditDowngradeRendersExternal(t *testing.T) {
	var out, errOut bytes.Buffer
	action := closure.Action{Verb: closure.Delete, Target: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web"}}
	clauses := []closure.ScopeClause{closure.OwnershipClause(action.Target)}
	dec := closure.Decision{
		Verdict: closure.Warn,
		Reason:  "affected-resource closure escapes task scope (audit: downgraded from BLOCK)",
		Escaping: []closure.Ref{
			{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web-svc"},
		},
		External: []closure.Ref{
			{GVK: closure.GVK{Kind: "External"}, Namespace: "prod", Name: "example.com/external-lb"},
		},
	}
	writeReport(&out, &errOut, action, clauses, "derived (ownership-tree)", dec, true)
	combined := out.String() + errOut.String()
	if !strings.Contains(combined, "Service/prod/web-svc") {
		t.Errorf("audit-downgrade report dropped the escaping ref; got stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
	if !strings.Contains(combined, "External/prod/example.com/external-lb") {
		t.Errorf("audit-downgrade report dropped the External (cross-boundary) ref; got stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
}

// TestParseActionDeleteCascade: parseAction turns a verb + already-split canonical
// Kind + name + namespace into a closure.Action with the verb, a Ref carrying the
// canonical Kind/ns/name (no UID — resolved later by the live State), and Cascade=true
// defaulted for delete (mirroring scenario.parseAction). The target split happens once
// in runCheckLive (see splitTarget), so parseAction takes the pieces, not "Kind/name".
func TestParseActionDeleteCascade(t *testing.T) {
	a, err := parseAction("delete", "Deployment", "web", "prod")
	if err != nil {
		t.Fatalf("parseAction(delete Deployment web -n prod) = %v, want nil", err)
	}
	if a.Verb != closure.Delete {
		t.Errorf("Verb = %q, want %q", a.Verb, closure.Delete)
	}
	if a.Target.GVK.Kind != "Deployment" || a.Target.Namespace != "prod" || a.Target.Name != "web" {
		t.Errorf("Target = %+v, want Deployment/prod/web", a.Target)
	}
	if a.Target.UID != "" {
		t.Errorf("Target.UID = %q, want empty (the live State resolves it by Kind/ns/name)", a.Target.UID)
	}
	if !a.Cascade {
		t.Errorf("Cascade = false, want true (a delete defaults to cascading)")
	}
}

// TestParseActionRejectsVerbs: unsupported verbs fail at the parse boundary rather
// than run a misread action. A mutation verb (update/patch) is rejected with a message
// naming the deferral (open Q4), distinct from an unknown verb.
func TestParseActionRejectsVerbs(t *testing.T) {
	cases := []struct {
		name, verb, wantSubstr string
	}{
		{"unknown verb", "frobnicate", "unknown verb"},
		{"mutation verb update", "update", "mutation"},
		{"mutation verb patch", "patch", "mutation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAction(tc.verb, "Deployment", "web", "prod")
			if err == nil {
				t.Fatalf("parseAction(%q) = nil error, want one mentioning %q", tc.verb, tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("parseAction error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestExtractNamespace: the kubectl-style trailing `-n` parser takes a normal value,
// errors on a bare trailing `-n` (no value), and — the hardening — errors when the
// value it would consume looks like a Kind/name target (contains "/"), rather than
// silently swallowing a misplaced target as the namespace.
func TestExtractNamespace(t *testing.T) {
	t.Run("normal -n ns", func(t *testing.T) {
		positional, ns, err := extractNamespace([]string{"delete", "Deployment/web", "-n", "prod"})
		if err != nil {
			t.Fatalf("extractNamespace = %v, want nil", err)
		}
		if ns != "prod" {
			t.Errorf("namespace = %q, want prod", ns)
		}
		if len(positional) != 2 || positional[0] != "delete" || positional[1] != "Deployment/web" {
			t.Errorf("positional = %v, want [delete Deployment/web]", positional)
		}
	})
	t.Run("bare trailing -n errors", func(t *testing.T) {
		if _, _, err := extractNamespace([]string{"delete", "Deployment/web", "-n"}); err == nil {
			t.Fatal("extractNamespace(bare -n) = nil error, want a missing-value error")
		}
	})
	t.Run("-n consuming a slash-bearing target errors", func(t *testing.T) {
		_, _, err := extractNamespace([]string{"delete", "-n", "Deployment/web"})
		if err == nil {
			t.Fatal("extractNamespace(-n Deployment/web) = nil error, want a looks-like-target error")
		}
		if !strings.Contains(err.Error(), "looks like") {
			t.Errorf("error = %q, want one mentioning the namespace looks like a target", err.Error())
		}
	})
}

// TestSplitTargetRejects: the single <Kind>/<name> split (done ONCE in runCheckLive)
// rejects a missing slash, an empty kind, or an empty name with a clear "invalid
// target" usage error, so a malformed target never reaches kind resolution or the
// engine. This is the one place the target form is validated (no duplicate re-check).
func TestSplitTargetRejects(t *testing.T) {
	for _, target := range []string{"Deployment", "Deployment/", "/web", "", ".apps/web", "Deployment./web"} {
		if _, _, _, err := splitTarget(target); err == nil {
			t.Errorf("splitTarget(%q) = nil error, want an invalid-target error", target)
		}
	}
	kind, group, name, err := splitTarget("Deployment/web")
	if err != nil {
		t.Fatalf("splitTarget(Deployment/web) = %v, want nil", err)
	}
	if kind != "Deployment" || group != "" || name != "web" {
		t.Errorf("splitTarget(Deployment/web) = (%q, %q, %q), want (Deployment, \"\", web)", kind, group, name)
	}
}

// TestSplitTargetParsesGroupQualifier (S2 #16): the target accepts an optional ".group"
// on the Kind — "<Kind>.<group>/<name>" — so an operator can disambiguate a Kind served
// by multiple API groups. The group is split on the FIRST ".", so a multi-dot group
// (example.com) is preserved.
func TestSplitTargetParsesGroupQualifier(t *testing.T) {
	cases := []struct{ in, kind, group, name string }{
		{"Deployment.apps/web", "Deployment", "apps", "web"},
		{"Cluster.infra.example.com/c1", "Cluster", "infra.example.com", "c1"},
		{"Deployment/web", "Deployment", "", "web"},
	}
	for _, c := range cases {
		kind, group, name, err := splitTarget(c.in)
		if err != nil {
			t.Fatalf("splitTarget(%q) = %v, want nil", c.in, err)
		}
		if kind != c.kind || group != c.group || name != c.name {
			t.Errorf("splitTarget(%q) = (%q, %q, %q), want (%q, %q, %q)", c.in, kind, group, name, c.kind, c.group, c.name)
		}
	}
}

// fakeLiveReader is a hermetic stand-in for cluster.Reader: it returns a prebuilt
// closure.State and a canonical Kind, so the live CLI path can be exercised end-to-end
// with no cluster. stateErr/kindErr model the slice-4 fail-closed inputs.
type fakeLiveReader struct {
	state    closure.State
	kind     string
	stateErr error
	kindErr  error
	// stateBlocks models a hung API server: State waits for the request context to be
	// cancelled (deadline or signal) and returns its error, so the S1 timeout path can be
	// exercised hermetically.
	stateBlocks bool
}

func (f *fakeLiveReader) State(ctx context.Context) (closure.State, error) {
	if f.stateBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.state, f.stateErr
}

func (f *fakeLiveReader) ResolveKind(_ context.Context, kind, _ string) (string, error) {
	if f.kindErr != nil {
		return "", f.kindErr
	}
	if f.kind != "" {
		return f.kind, nil
	}
	return kind, nil
}

// withFakeLiveReader swaps the live-reader constructor for one returning fr, so a
// test drives the live path without a *rest.Config or a cluster. It restores the
// original on cleanup.
func withFakeLiveReader(t *testing.T, fr *fakeLiveReader) {
	t.Helper()
	orig := newLiveReader
	newLiveReader = func(_, _ string) (liveReader, error) { return fr, nil }
	t.Cleanup(func() { newLiveReader = orig })
}

// liveState builds a State equivalent to scenario 01's intra-namespace cascade:
// a Deployment owning a ReplicaSet owning two Pods, plus an un-owned Service. A
// cascading delete of the Deployment escapes the derived ownership tree via the
// Service (and the State has no UID-less ambiguity because every object has a uid).
func liveStateScenario01() closure.State {
	dep := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-dep"}
	rs := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-rs", UID: "uid-rs"}
	pod1 := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1", UID: "uid-p1"}
	svc := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web-svc", UID: "uid-svc"}
	return closure.NewScanState([]closure.Object{
		{Ref: dep},
		{Ref: rs, Owners: []closure.OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid-dep"}}},
		{Ref: pod1, Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-rs", UID: "uid-rs"}}, Labels: map[string]string{"app": "web"}},
		// The Service selects the pod but is NOT owned by the Deployment, so it escapes
		// the derived ownership tree.
		{Ref: svc, Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
	})
}

// TestCheckLivePathDerivedScopeBlocks: with a cluster flag present the live path is
// selected; it resolves the target Kind, builds the live State, DERIVES the Level-0
// ownership scope (no contract), runs the shared Safe/mode/writeReport, and renders
// the derived provenance. Under --mode enforce the un-owned Service escapes the
// derived tree → BLOCK (errBlocked / exit 2).
func TestCheckLivePathDerivedScopeBlocks(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{state: liveStateScenario01(), kind: "Deployment"})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "--mode", "enforce", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("live check (enforce) err = %v, want errBlocked (Service escapes derived tree)", err)
	}
	stdout := out.String()
	for _, want := range []string{
		// The CLI target carries only the canonical Kind (no API group — the operator
		// types "Deployment/web"); the engine resolves it by its human key (Kind/ns/name,
		// group-ignored), so the derived ownership clause renders group-less.
		"owns:Deployment/prod/web", // derived ownership clause rooted at the target
		"derived (ownership-tree)", // derived provenance reported
		"BLOCK",
		"Service/prod/web-svc", // the un-owned Service escapes
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("live BLOCK report missing %q; got:\n%s", want, stdout)
		}
	}
}

// TestCheckLivePathSelectionByKubeconfigFlag: --kubeconfig alone (no --context) also
// selects the live path; the scenario-dir path is never taken (no positional dir).
func TestCheckLivePathSelectionByKubeconfigFlag(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{state: liveStateScenario01(), kind: "Deployment"})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--kubeconfig", "/no/such/file", "--mode", "enforce", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("live check via --kubeconfig err = %v, want errBlocked", err)
	}
	if !strings.Contains(out.String(), "BLOCK") {
		t.Errorf("live path not taken via --kubeconfig; got:\n%s", out.String())
	}
}

// TestCheckLiveUnresolvableTarget: the cluster lists objects but NOT the named target.
// closure.Safe's empty-closure fail-closed deny fires → BLOCK with a "fail-closed"
// reason and errBlocked (exit 2). It is distinct from an unreadable-kind error, and it
// is NOT softened by audit mode (a fail-closed Block, len(Escaping)==0, stays a deny).
func TestCheckLiveUnresolvableTarget(t *testing.T) {
	// A State with one unrelated Pod — the Deployment/web target is absent.
	state := closure.NewScanState([]closure.Object{
		{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "other", UID: "uid-x"}},
	})
	withFakeLiveReader(t, &fakeLiveReader{state: state, kind: "Deployment"})

	// Default mode (audit): a fail-closed Block must NOT be downgraded to WARN.
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("unresolvable target err = %v, want errBlocked (fail-closed, even in audit)", err)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "fail-closed") {
		t.Errorf("stdout missing fail-closed reason; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "BLOCK") {
		t.Errorf("fail-closed target-not-found should render BLOCK, not WARN; got:\n%s", stdout)
	}
}

// TestCheckLiveUnreadableKind: a forbidden/unreadable kind makes the reader's State
// return an error; the live path must DENY THE WHOLE CHECK fail-closed (exit 1, not
// errBlocked), never proceeding on a partial snapshot. The reason names the cluster-read
// failure and is distinguishable from an unresolvable target (which is exit 2 / BLOCK).
func TestCheckLiveUnreadableKind(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{
		kind:     "Deployment",
		stateErr: errors.New("list secrets: secrets is forbidden: User cannot list resource secrets"),
	})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if err == nil {
		t.Fatal("unreadable kind = nil error, want a fail-closed error")
	}
	if errors.Is(err, errBlocked) {
		t.Errorf("a cluster-read failure must be exit 1 (operational), not errBlocked/exit 2; got %v", err)
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Errorf("error must name the fail-closed deny; got %v", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error should surface the underlying read failure; got %v", err)
	}
}

// TestCheckLiveDiscoveryFailure: a discovery failure (here surfaced via ResolveKind,
// which queries discovery first) fails closed with a distinct reason — it never falls
// back to a static kind map. Exit 1 (operational), not errBlocked.
func TestCheckLiveDiscoveryFailure(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{
		kindErr: errors.New("discover server resources: the server could not find the requested resource"),
	})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if err == nil {
		t.Fatal("discovery failure = nil error, want a fail-closed error")
	}
	if errors.Is(err, errBlocked) {
		t.Errorf("a discovery failure must be exit 1 (operational), not errBlocked/exit 2; got %v", err)
	}
	if !strings.Contains(err.Error(), "discover server resources") {
		t.Errorf("error should surface the discovery failure distinctly; got %v", err)
	}
}

// TestCheckLiveTimeoutFailsClosed (S1): a live read that outlives --timeout must DENY the
// whole check fail-closed (exit 1, not errBlocked), never proceeding on a partial snapshot.
// The fake reader's State blocks on the request context, so a tiny --timeout fires the
// deadline; runCheckLive returns the cluster-read fail-closed error and writes no report.
func TestCheckLiveTimeoutFailsClosed(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{kind: "Deployment", stateBlocks: true})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "--timeout", "1ms", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if err == nil {
		t.Fatal("timed-out read = nil error, want a fail-closed error")
	}
	if errors.Is(err, errBlocked) {
		t.Errorf("a timed-out read must be exit 1 (operational), not errBlocked/exit 2; got %v", err)
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Errorf("error must name the fail-closed deny; got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no report should be written on a fail-closed timeout; got stdout:\n%s", out.String())
	}
}

// TestCheckLiveTimeoutZeroDisablesDeadline (S1): --timeout 0 means "no deadline"
// (signal-cancel only); it must NOT instantly deadline, so a normal read completes and
// renders its verdict as usual.
func TestCheckLiveTimeoutZeroDisablesDeadline(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{state: liveStateScenario01(), kind: "Deployment"})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "--mode", "enforce", "--timeout", "0", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("--timeout 0 should disable the deadline and run normally; err = %v, want errBlocked", err)
	}
	if !strings.Contains(out.String(), "BLOCK") {
		t.Errorf("--timeout 0 normal run should render the verdict; got:\n%s", out.String())
	}
}

// TestCheckTimeoutRejectsBadDuration (S1): a malformed --timeout is a usage error (exit 1),
// not a silent default — the operator's invocation must be rejected, not misread.
func TestCheckTimeoutRejectsBadDuration(t *testing.T) {
	withFakeLiveReader(t, &fakeLiveReader{state: liveStateScenario01(), kind: "Deployment"})
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--context", "kind-krsm", "--timeout", "nope", "delete", "Deployment/web", "-n", "prod"}, &out, &errOut)
	if err == nil {
		t.Fatal("malformed --timeout = nil error, want a usage error")
	}
	if errors.Is(err, errBlocked) {
		t.Errorf("a malformed --timeout is a usage error (exit 1), not errBlocked/exit 2; got %v", err)
	}
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
	// --mode enforce: audit (the default) would downgrade this scope escape to WARN.
	err := run([]string{"check", "--mode", "enforce", scenarioDir("01-memory-pressure-cascade")}, &out, &errOut)
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
		t.Errorf("stderr missing external detail; got:\n%s", errOut.String())
	}
	// stdout stays self-contained: a WARN verdict stub, but the external detail
	// itself (which needs live-cluster confirmation) stays on stderr.
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("stdout missing WARN verdict stub; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "External/prod/example.com/external-lb") {
		t.Errorf("stdout must not carry the external detail; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "CLOSURE") {
		t.Errorf("stdout should still carry the factual report; got:\n%s", out.String())
	}
}

// TestCheckPlainNoEmoji: --plain emits ASCII only (no emoji), still BLOCK + sentinel.
func TestCheckPlainNoEmoji(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--plain", "--mode", "enforce", scenarioDir("01-memory-pressure-cascade")}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("run(check --plain --mode enforce, block) err = %v, want errBlocked", err)
	}
	if !strings.Contains(out.String(), "BLOCK") {
		t.Errorf("stdout missing BLOCK verdict; got:\n%s", out.String())
	}
	for _, emoji := range []string{"❌", "⚠", "✅"} {
		if strings.Contains(out.String(), emoji) {
			t.Errorf("--plain output still contains emoji %q; got:\n%s", emoji, out.String())
		}
	}
}

// TestCheckHelpFlag: -h / --help prints check usage to stdout and exits 0 (no load).
func TestCheckHelpFlag(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var out, errOut bytes.Buffer
		if err := run([]string{"check", arg}, &out, &errOut); err != nil {
			t.Fatalf("run(check %s) = %v, want nil", arg, err)
		}
		for _, want := range []string{"check", "--plain"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("check %s usage missing %q; got:\n%s", arg, want, out.String())
			}
		}
	}
}

// TestCheckBadFlag: an unknown flag is a usage error (exit 1), not a Block.
func TestCheckBadFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--bogus", scenarioDir("01-memory-pressure-cascade")}, &out, &errOut)
	if err == nil {
		t.Fatal("run(check --bogus) = nil error, want a usage error")
	}
	if errors.Is(err, errBlocked) {
		t.Error("bad-flag error must not be errBlocked (exit 1, not 2)")
	}
}

// TestCheckExtraArgIsUsageError: a stray positional argument after <dir> is a
// usage error (exit 1), not a silently-ignored token. Guards against a flag placed
// after the dir (e.g. `check <dir> --plain`) being dropped without warning.
func TestCheckExtraArgIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", scenarioDir("01-memory-pressure-cascade"), "extra"}, &out, &errOut)
	if err == nil {
		t.Fatal("run(check dir extra) = nil error, want a usage error")
	}
	if errors.Is(err, errBlocked) {
		t.Error("extra-arg error must not be errBlocked (exit 1, not 2)")
	}
}

// TestScopeStrIncludesGroup: a scope clause with an API group renders as
// Kind.group so clauses differing only by group are unambiguous; core stays bare.
func TestScopeStrIncludesGroup(t *testing.T) {
	grouped := closure.ScopeClause{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web"}
	if got, want := scopeStr(grouped), "Deployment.apps/prod/web"; got != want {
		t.Errorf("scopeStr(grouped) = %q, want %q", got, want)
	}
	core := closure.ScopeClause{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1"}
	if got, want := scopeStr(core), "Pod/prod/web-1"; got != want {
		t.Errorf("scopeStr(core) = %q, want %q", got, want)
	}
}

// TestScopeStrOwnershipAndNamespace: the ownership and namespace dimensions render
// readably on the SCOPE line — ownership as owns:<Kind[.group]>/<ns>/<name> from the
// clause Root, namespace as ns:<ns>/* (or ns:<Kind[.group]>/<ns>/* when GVK is set).
func TestScopeStrOwnershipAndNamespace(t *testing.T) {
	owns := closure.OwnershipClause(closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web"})
	if got, want := scopeStr(owns), "owns:Deployment.apps/prod/web"; got != want {
		t.Errorf("scopeStr(ownership) = %q, want %q", got, want)
	}
	ns := closure.NamespaceClause(closure.GVK{}, "prod")
	if got, want := scopeStr(ns), "ns:prod/*"; got != want {
		t.Errorf("scopeStr(namespace) = %q, want %q", got, want)
	}
	nsGVK := closure.NamespaceClause(closure.GVK{Version: "v1", Kind: "Pod"}, "prod")
	if got, want := scopeStr(nsGVK), "ns:Pod/prod/*"; got != want {
		t.Errorf("scopeStr(namespace+gvk) = %q, want %q", got, want)
	}
}

// TestCheckDerivedProvenance: a scenario with NO scope.yaml and NO taskcontract.yaml
// derives a Level-0 ownership scope from the request target. The SCOPE line renders
// the ownership clause (owns:…) and shows the derived provenance; the un-owned Service
// escapes the tree → BLOCK (scenario-01-style cascade).
func TestCheckDerivedProvenance(t *testing.T) {
	var out, errOut bytes.Buffer
	// --mode enforce so the derived-tree escape stays a BLOCK; under the audit default
	// it would be downgraded to WARN (asserted separately in TestCheckModeAuditDowngrades).
	err := run([]string{"check", "--mode", "enforce", scenarioDir("26-derived-default")}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("run(check --mode enforce, derived scenario) err = %v, want errBlocked (Service escapes derived tree)", err)
	}
	stdout := out.String()
	for _, want := range []string{
		"owns:Deployment.apps/prod/web", // derived ownership clause renders readably
		"derived (ownership-tree)",      // provenance is reported
		"BLOCK",
		"Service/prod/web-svc", // the un-owned Service escapes
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, stdout)
		}
	}
}

// TestCheckModeEnforceBlocks: an escaping scenario under --mode enforce renders
// BLOCK and signals the block via errBlocked (exit 2) — the same verdict closure.Safe
// produced, unsoftened.
func TestCheckModeEnforceBlocks(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--mode", "enforce", scenarioDir("26-derived-default")}, &out, &errOut)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("run(check --mode enforce, escape scenario) err = %v, want errBlocked", err)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "BLOCK") {
		t.Errorf("stdout missing BLOCK verdict; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Service/prod/web-svc") {
		t.Errorf("stdout missing escaping ref; got:\n%s", stdout)
	}
}

// TestCheckModeAuditDowngrades: the SAME escaping scenario under --mode audit is
// downgraded to WARN (exit 0, no errBlocked), still shows the escaping detail, and
// marks the audit downgrade in the verdict line.
func TestCheckModeAuditDowngrades(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--mode", "audit", scenarioDir("26-derived-default")}, &out, &errOut)
	if err != nil {
		t.Fatalf("run(check --mode audit, escape scenario) = %v, want nil (downgraded to WARN, exit 0)", err)
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("stdout missing WARN verdict (audit downgrade); got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "audit") {
		t.Errorf("stdout does not mark the audit downgrade in the verdict line; got:\n%s", out.String())
	}
	// The escaping detail must still be visible so the operator sees what would block.
	if !strings.Contains(out.String()+errOut.String(), "Service/prod/web-svc") {
		t.Errorf("audit dropped the escaping detail; stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
}

// TestCheckModeDefaultsToAudit: with no --mode flag, an escaping scenario is treated
// as audit (WARN, exit 0) — the install default that does not deny on a day-0 escape.
func TestCheckModeDefaultsToAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", scenarioDir("26-derived-default")}, &out, &errOut)
	if err != nil {
		t.Fatalf("run(check, escape scenario, default mode) = %v, want nil (default audit → WARN)", err)
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("default mode is not audit (expected WARN downgrade); got:\n%s", out.String())
	}
}

// TestCheckModeInvalid: an unrecognised --mode value is a usage error (exit 1), not a
// Block (exit 2). The error names the offending value and the valid set.
func TestCheckModeInvalid(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"check", "--mode", "bogus", scenarioDir("26-derived-default")}, &out, &errOut)
	if err == nil {
		t.Fatal("run(check --mode bogus) = nil error, want a usage error")
	}
	if errors.Is(err, errBlocked) {
		t.Error("invalid-mode error must not be errBlocked (exit 1, not 2)")
	}
}

// TestCheckHelpListsMode: -h / --help mentions the --mode flag so an operator can
// discover the audit/enforce switch.
func TestCheckHelpListsMode(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"check", "--help"}, &out, &errOut); err != nil {
		t.Fatalf("run(check --help) = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "--mode") {
		t.Errorf("check usage missing --mode; got:\n%s", out.String())
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

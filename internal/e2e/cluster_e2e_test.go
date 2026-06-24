//go:build cluster

// Package e2e holds the v0.4 "Done when" acceptance test (ROADMAP v0.4): it proves,
// on a REAL kind cluster with NO cluster.yaml and NO contract, that `krsm check` on
// the live path reports the correct closure for a real action and flags an escaping
// action under the DERIVED default scope — while an in-scope action is allowed.
//
// It is gated behind `//go:build cluster` so the default hermetic gate (`make check`,
// CI `go test`) never runs it; `make e2e` builds the binary and runs it with
// `-tags cluster`. It stands up its own ephemeral kind cluster and tears it down in
// cleanup (even on failure), so the run is self-contained.
//
// krsm itself reads the cluster READ-ONLY (the kubectl apply below is test SETUP, not
// part of krsm); the read-only posture is guarded separately by internal/cluster's
// TestReaderUsesOnlyReadVerbs / TestPackageInvokesNoWriteVerbs.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// kindClusterName is the ephemeral cluster's name; kind exposes it as the
	// kubeconfig context "kind-<name>", which krsm targets via --context.
	kindClusterName = "krsm-e2e"
	kindContext     = "kind-" + kindClusterName
)

// escapeManifests model scenario 01 (the intra-namespace cascade): a Deployment whose
// pods are also selected by a Service. The Service is NOT owned by the Deployment, so
// a cascading delete pulls it into the closure but it lies OUTSIDE the target's
// ownership tree → it ESCAPES the derived ownership scope → must be FLAGGED.
const escapeManifests = `
apiVersion: v1
kind: Namespace
metadata:
  name: escape
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: escape
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
---
apiVersion: v1
kind: Service
metadata:
  name: web-svc
  namespace: escape
spec:
  selector: {app: web}
  ports:
    - port: 80
`

// inScopeManifests model a self-contained ownership tree: a Deployment whose cascade
// stays entirely within its own owned subtree — no Service selector reaching its pods,
// no cross-ref to un-owned collateral. The cascade closure ⊆ derived ownership scope
// → ALLOW.
const inScopeManifests = `
apiVersion: v1
kind: Namespace
metadata:
  name: selfcontained
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: solo
  namespace: selfcontained
spec:
  replicas: 2
  selector:
    matchLabels: {app: solo}
  template:
    metadata:
      labels: {app: solo}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
`

// TestKindDerivedScopeAcceptance is the v0.4 acceptance test. It:
//
//	(a) creates an ephemeral kind cluster (deleted in cleanup, even on failure),
//	(b) applies two fixtures with kubectl and waits for the workloads' pods/ReplicaSets
//	    to exist (so the live ownerReference cascade is real),
//	(c) runs the REAL built krsm binary against the live cluster — NO cluster.yaml, NO
//	    contract — for both an escaping and an in-scope cascading delete,
//	(d) asserts: the escaping `delete Deployment/web` is FLAGGED under --mode enforce
//	    (exit 2, BLOCK, the un-owned Service named among the escaping resources), and
//	    the in-scope `delete Deployment/solo` is ALLOWED (exit 0).
func TestKindDerivedScopeAcceptance(t *testing.T) {
	requireBinary(t, "kind")
	requireBinary(t, "kubectl")
	binary := buildKrsm(t)

	createKindCluster(t)

	kubectlApply(t, escapeManifests)
	kubectlApply(t, inScopeManifests)
	// The Deployment controller must have created the ReplicaSet + Pods so the live
	// ownerReference cascade (Deployment→ReplicaSet→Pods) is present for the closure.
	waitForPods(t, "escape", "app=web", 2)
	waitForPods(t, "selfcontained", "app=solo", 2)

	// Escaping case under --mode enforce: the un-owned Service escapes the derived
	// ownership tree → BLOCK, exit 2.
	t.Run("escaping action is flagged under enforce", func(t *testing.T) {
		stdout, stderr, code := runKrsm(t, binary, "check", "--context", kindContext, "--mode", "enforce", "delete", "Deployment/web", "-n", "escape")
		t.Logf("krsm (escape) exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		if code != 2 {
			t.Fatalf("escaping delete exit = %d, want 2 (BLOCK under enforce); stdout:\n%s", code, stdout)
		}
		if !strings.Contains(stdout, "BLOCK") {
			t.Errorf("escaping report missing BLOCK; got:\n%s", stdout)
		}
		// The Service is the collateral OUTSIDE the ownership tree — it must be named
		// among the flagged escaping resources, proving the derived scope is the
		// ownership tree (not a bare namespace, which would Allow it).
		if !strings.Contains(stdout, "Service/escape/web-svc") {
			t.Errorf("escaping report does not flag the un-owned Service; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, "derived (ownership-tree)") {
			t.Errorf("escaping report does not show the derived provenance; got:\n%s", stdout)
		}
	})

	// In-scope case: the cascade stays within the ownership tree → ALLOW, exit 0.
	t.Run("in-scope action is allowed", func(t *testing.T) {
		stdout, stderr, code := runKrsm(t, binary, "check", "--context", kindContext, "--mode", "enforce", "delete", "Deployment/solo", "-n", "selfcontained")
		t.Logf("krsm (in-scope) exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		if code != 0 {
			t.Fatalf("in-scope delete exit = %d, want 0 (ALLOW); stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "ALLOW") {
			t.Errorf("in-scope report missing ALLOW; got:\n%s", stdout)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("%s not found in PATH: %v (the cluster e2e needs %s installed)", name, err, name)
	}
}

// buildKrsm builds the krsm binary into a temp dir and returns its path, so the test
// runs the REAL command-line tool (not an in-process call).
func buildKrsm(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	out := filepath.Join(t.TempDir(), "krsm")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/krsm")
	cmd.Dir = repoRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build krsm: %v\n%s", err, b)
	}
	return out
}

// createKindCluster creates the ephemeral cluster and registers a teardown that runs
// even on test failure, so a crashed run never leaks a docker-backed cluster.
func createKindCluster(t *testing.T) {
	t.Helper()
	// Delete any leftover cluster from a previous aborted run first (idempotent).
	_ = exec.Command("kind", "delete", "cluster", "--name", kindClusterName).Run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kind", "create", "cluster", "--name", kindClusterName, "--wait", "120s")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kind create cluster: %v\n%s", err, b)
	}
	t.Cleanup(func() {
		if b, err := exec.Command("kind", "delete", "cluster", "--name", kindClusterName).CombinedOutput(); err != nil {
			t.Logf("kind delete cluster (cleanup) failed: %v\n%s", err, b)
		}
	})
}

func kubectlApply(t *testing.T, manifests string) {
	t.Helper()
	cmd := exec.Command("kubectl", "--context", kindContext, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, b)
	}
}

// waitForPods blocks until at least want pods match the label selector in ns, so the
// live ownerReference cascade (Deployment→ReplicaSet→Pods) is materialised before krsm
// reads the cluster. It uses `kubectl wait` for readiness with a bounded timeout.
func waitForPods(t *testing.T, ns, selector string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := exec.Command("kubectl", "--context", kindContext, "-n", ns,
			"get", "pods", "-l", selector, "--no-headers").CombinedOutput()
		if err == nil {
			lines := 0
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(ln) != "" {
					lines++
				}
			}
			if lines >= want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for >=%d pods matching %q in %s", want, selector, ns)
}

// runKrsm runs the built binary and returns stdout, stderr and the exit code. A
// non-zero exit is NOT a test failure here — the exit code is the assertion subject
// (0 allow, 2 block).
func runKrsm(t *testing.T, binary string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := exec.Command(binary, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run krsm %v: %v", args, err)
		}
	}
	return out.String(), errBuf.String(), code
}

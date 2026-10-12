package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ridik-il/krsm/scope"
)

// serveRejects runs `krsm serve` with dummy TLS paths, no kubeconfig and the given
// flags, and requires a usage error that names wantFlag — returned by runServe's
// fail-fast block BEFORE serveWebhook (the first cluster contact) is reached. An
// unserveable configuration must be reported in milliseconds, not after the operator has
// waited out an informer sync (review-1 entry 16).
func serveRejects(t *testing.T, wantFlag string, args ...string) {
	t.Helper()
	previous := serveWebhook
	t.Cleanup(func() { serveWebhook = previous })
	serveWebhook = func(serveOpts, scope.Mode, io.Writer, io.Writer) error {
		t.Errorf("serve %v: must be refused before any cluster contact, but serveWebhook was reached", args)
		return nil
	}
	err := runServe(append([]string{"--tls-cert", "c", "--tls-key", "k"}, args...), io.Discard, io.Discard)
	if err == nil {
		t.Errorf("serve %v: want a usage error, got nil", args)
		return
	}
	if !strings.Contains(err.Error(), wantFlag) {
		t.Errorf("serve %v: error %q must name the offending flag %s", args, err, wantFlag)
	}
}

// TestServeRejectsEmptyAnnotationKeys (review-1 entry 12): an empty scope-channel key
// used to fall silently back to the default — an operator who cleared the flag believing
// they had DISABLED a channel would in fact have left it live on krsm.io/. Refused.
func TestServeRejectsEmptyAnnotationKeys(t *testing.T) {
	serveRejects(t, "--target-annotation", "--target-annotation", "")
	serveRejects(t, "--scope-annotation", "--scope-annotation", "")
}

// TestServeRejectsMalformedAnnotationKeys (review-1 entry 13): a key that is not a valid
// annotation key ([<dns-subdomain>/]<name>, the k8s qualified-name rules) can never be
// present on an object, so the channel it names would be silently dead.
func TestServeRejectsMalformedAnnotationKeys(t *testing.T) {
	for _, key := range []string{"bad key/", "-x", "a/b/c", "example.com/"} {
		serveRejects(t, "--target-annotation", "--target-annotation", key)
		serveRejects(t, "--scope-annotation", "--scope-annotation", key)
	}
}

// TestServeRejectsCollidingAnnotationKeys (review-1 entry 14): the target, scope and
// agent-identity channels must be three distinct keys. One key serving two channels makes
// every value parse as both — webhook.New refuses target==scope too, but only after the
// informers have synced, and it cannot see the agent key at all. An EMPTY agent key
// disables annotation matching, so it legitimately collides with nothing.
func TestServeRejectsCollidingAnnotationKeys(t *testing.T) {
	serveRejects(t, "--scope-annotation", "--target-annotation", "example.com/k", "--scope-annotation", "example.com/k")
	serveRejects(t, "--agent-annotation", "--target-annotation", "example.com/k", "--agent-annotation", "example.com/k")
	serveRejects(t, "--agent-annotation", "--scope-annotation", "example.com/k", "--agent-annotation", "example.com/k")
	// Half-configured onto another channel's DEFAULT is a collision too.
	serveRejects(t, "--agent-annotation", "--agent-annotation", "krsm.io/target")

	// An empty agent key is "annotation matching off", not a key — it collides with
	// nothing, so a serviceaccount-gated server with default channels starts.
	o := captureServeOpts(t, "--agent-annotation", "", "--agent-serviceaccount", "system:serviceaccount:agents:a")
	if o.agentAnnotation != "" {
		t.Errorf("agentAnnotation = %q, want empty", o.agentAnnotation)
	}
}

// TestServeRejectsWildcardAndEmptyAllowlistEntries (review-1 entry 15): the allowlist is
// matched literally, so `*` would exempt nothing while the operator believes it exempts
// everything — and an empty entry is either a typo or a generator bug, never a namespace.
// Both are usage errors rather than silently dead (or silently dropped) exemptions.
func TestServeRejectsWildcardAndEmptyAllowlistEntries(t *testing.T) {
	serveRejects(t, "--allow-namespace", "--allow-namespace", "*")
	serveRejects(t, "--allow-namespace", "--allow-namespace", "a,,b")
	serveRejects(t, "--allow-namespace", "--allow-namespace", "a,b,")
	serveRejects(t, "--allow-cluster-scoped-kind", "--allow-cluster-scoped-kind", "*")
	serveRejects(t, "--allow-cluster-scoped-kind", "--allow-cluster-scoped-kind", "Node,,Foo.example.com")

	// An UNSET flag is no allowlist at all, not one empty entry.
	captureServeOpts(t)
}

// serveStderr runs `krsm serve` with the given flags up to (and not into) serveWebhook
// and returns what it wrote to stderr — the startup log an operator reads.
func serveStderr(t *testing.T, args ...string) string {
	t.Helper()
	previous := serveWebhook
	t.Cleanup(func() { serveWebhook = previous })
	serveWebhook = func(serveOpts, scope.Mode, io.Writer, io.Writer) error { return nil }
	var stderr bytes.Buffer
	if err := runServe(append([]string{"--tls-cert", "c", "--tls-key", "k"}, args...), io.Discard, &stderr); err != nil {
		t.Fatalf("runServe(%v): %v", args, err)
	}
	return stderr.String()
}

// TestServeWarnsWhenAScopeChannelIsReKeyed (review-1 entry 17): moving a channel off its
// default key makes the OLD key inert, so every object still annotated with it silently
// falls to L0 — which may be WIDER than the re-root or contract it named, not only
// narrower. That is a startup fact an operator must see: one warning per moved key,
// naming the old key, the new key and L0. Default keys log nothing.
func TestServeWarnsWhenAScopeChannelIsReKeyed(t *testing.T) {
	out := serveStderr(t, "--scope-annotation", "example.com/scope")
	if n := strings.Count(out, "warning"); n != 1 {
		t.Fatalf("want exactly one warning for one re-keyed channel, got %d in %q", n, out)
	}
	for _, want := range []string{"krsm.io/scope", "example.com/scope", "L0"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning %q must name %q", out, want)
		}
	}

	out = serveStderr(t, "--target-annotation", "example.com/target", "--scope-annotation", "example.com/scope")
	if n := strings.Count(out, "warning"); n != 2 {
		t.Errorf("want one warning per re-keyed channel (2), got %d in %q", n, out)
	}
	if !strings.Contains(out, "krsm.io/target") || !strings.Contains(out, "example.com/target") {
		t.Errorf("the target-channel warning must name both keys, got %q", out)
	}

	if out := serveStderr(t); out != "" {
		t.Errorf("default keys must log nothing, got %q", out)
	}
}

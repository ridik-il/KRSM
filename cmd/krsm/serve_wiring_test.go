package main

// Regression tests for PR #36 review finding 9: the production webhook.Config
// assembly must be under hermetic test — webhook.New tolerates a nil Fresh (guard
// disabled), so only these tests catch a dropped field before it silently disables
// the staleness guard in a cluster.

import (
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/ridik-il/krsm/scope"
	"github.com/ridik-il/krsm/state"
	"github.com/ridik-il/krsm/webhook"
)

// TestBuildWebhookConfig: every collaborator the webhook needs to fail closed is
// wired from the Provider, and the flags flow through unchanged.
func TestBuildWebhookConfig(t *testing.T) {
	o := serveOpts{
		agentAnnotation:      "krsm.io/task",
		agentServiceAccounts: "system:serviceaccount:agents:remediator",
		requestTimeout:       7 * time.Second,
	}
	cfg := buildWebhookConfig(o, scope.ModeEnforce, (*state.Provider)(nil))

	if cfg.State == nil || cfg.ScopeInfo == nil || cfg.Synced == nil {
		t.Error("Config must wire State, ScopeInfo and Synced from the Provider")
	}
	if cfg.Fresh == nil {
		t.Error("Config.Fresh must be wired — a nil Fresh silently disables the staleness guard (C2)")
	}
	if cfg.Mode != scope.ModeEnforce {
		t.Errorf("Mode = %q, want enforce", cfg.Mode)
	}
	if cfg.Timeout != o.requestTimeout {
		t.Errorf("Timeout = %v, want the --request-timeout value %v", cfg.Timeout, o.requestTimeout)
	}
	if _, ok := cfg.Matcher.(webhook.AnyMatcher); !ok {
		t.Errorf("Matcher = %T, want AnyMatcher (serviceaccount OR annotation) when --agent-serviceaccount is set", cfg.Matcher)
	}

	o.agentServiceAccounts = ""
	cfg = buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil))
	if m, ok := cfg.Matcher.(webhook.AnnotationMatcher); !ok || m.Key != "krsm.io/task" {
		t.Errorf("Matcher = %#v, want AnnotationMatcher{krsm.io/task} without --agent-serviceaccount", cfg.Matcher)
	}
}

// TestAgentMatcherIdentity: the assembled matcher gates the named serviceaccounts by
// request identity (payload-free — the scale/eviction/CONNECT signal) and everyone
// else only via the annotation.
func TestAgentMatcherIdentity(t *testing.T) {
	m := agentMatcher("krsm.io/task", "system:serviceaccount:agents:remediator,system:serviceaccount:agents:scaler")

	agent := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:agents:scaler"}}
	if !m.Matches(agent, nil, nil) {
		t.Error("a listed serviceaccount must match with no payload at all")
	}
	human := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: "kubernetes-admin"}}
	if m.Matches(human, nil, nil) {
		t.Error("an unlisted identity without the annotation must not match")
	}
}

package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// TestSlice6DoneWhen encodes issue #24's "Done when" literally, over ONE escaping action:
// deleting ReplicaSet prod/web-1 reaches Service prod/svc, which is outside the
// ReplicaSet's own ownership tree but inside Deployment prod/web's. The verdict and the
// reported provenance must follow the scope channel the request used.
//
//   - Allowed under a contract that authorises the tree.
//   - Warned under derived audit, and Denied under derived enforce.
//   - krsm.io/target re-roots the derivation at the Deployment.
//   - The allowlist exempts an allowlisted namespace in derived mode, but not under a
//     contract.
//
// The provenance appears in the AuditAnnotations of every decided response, and in the
// warning or deny text of every non-Allow one.
func TestSlice6DoneWhen(t *testing.T) {
	objs := reRootObjects()
	tree := taskContractCR(t, "prod", "tree",
		`{"dim":"ownership","root":{"group":"apps","version":"v1","kind":"Deployment","name":"web"}}`)
	narrow := taskContractCR(t, "prod", "narrow",
		`{"dim":"ownership","root":{"group":"apps","version":"v1","kind":"ReplicaSet","name":"web-1"}}`)
	sharedProd := Allowlist{Namespaces: map[string]bool{"prod": true}}

	for _, tc := range []struct {
		name        string
		mode        scope.Mode
		annotations string
		opts        []func(*Config)
		wantAllowed bool
		wantWarn    bool // allowed, but with a warning naming the escape
		wantProv    scope.Provenance
	}{
		{"contract authorising the tree allows", scope.ModeEnforce,
			annotations(DefaultScopeAnnotation, "prod/tree"),
			[]func(*Config){withContracts(contractsWith(tree))}, true, false, scope.ProvenanceContract},
		{"derived audit warns", scope.ModeAudit, "", nil, true, true, scope.ProvenanceDerivedOwner},
		{"derived enforce denies", scope.ModeEnforce, "", nil, false, false, scope.ProvenanceDerivedOwner},
		{"krsm.io/target re-roots at the Deployment", scope.ModeEnforce,
			annotations(DefaultTargetAnnotation, "Deployment/prod/web"), nil, true, false, scope.ProvenanceAnnotation},
		{"allowlist exempts the namespace in derived mode", scope.ModeEnforce, "",
			[]func(*Config){withAllowlist(sharedProd)}, true, false, scope.ProvenanceDerivedOwner},
		{"allowlist is ignored under a contract", scope.ModeEnforce,
			annotations(DefaultScopeAnnotation, "prod/narrow"),
			[]func(*Config){withAllowlist(sharedProd), withContracts(contractsWith(narrow))}, false, false, scope.ProvenanceContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]func(*Config){withObjects(objs)}, tc.opts...)
			s := newTestServer(t, closure.NewScanState(objs), tc.mode, opts...)
			out := s.Handle(context.Background(), rsDeleteReview("done-when", tc.annotations))

			if out.Response.Allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v (message %q, warnings %q)",
					out.Response.Allowed, tc.wantAllowed, resultMessage(out.Response), out.Response.Warnings)
			}
			if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(tc.wantProv) {
				t.Errorf("AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, tc.wantProv)
			}

			warn := strings.Join(out.Response.Warnings, "\n")
			switch {
			case !tc.wantAllowed:
				msg := resultMessage(out.Response)
				if !strings.Contains(msg, string(tc.wantProv)) || !strings.Contains(msg, "Service/prod/svc") {
					t.Errorf("deny %q must carry the provenance %q and name the escape", msg, tc.wantProv)
				}
			case tc.wantWarn:
				if !strings.Contains(warn, string(tc.wantProv)) || !strings.Contains(warn, "svc") {
					t.Errorf("warnings %q must carry the provenance %q and name the escape", warn, tc.wantProv)
				}
			default:
				if warn != "" {
					t.Errorf("a clean allow must carry no warnings, got %q", warn)
				}
			}
		})
	}
}

// resultMessage returns the deny text of a response, or "" when there is none.
func resultMessage(r *admissionv1.AdmissionResponse) string {
	if r == nil || r.Result == nil {
		return ""
	}
	return r.Result.Message
}

package closure

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// TestClosurePropertiesOnRandomGraphs is the generative backing for the
// computability/termination results (DESIGN §4, §8, §11): on randomly generated
// adversarial cluster graphs the closure walk must always
//
//   - terminate (the visited-set guard makes |C| finite even on cycles), and
//   - stay bounded by inventory: |C| ≤ |R|, and
//   - contain only real objects (C ⊆ R), with no duplicates.
//
// The generator deliberately manufactures the structures that break a naive
// walk: ownerReferences drawn uniformly over all objects (so self-loops, 2-cycles
// and deep chains all occur), selector/matchExpressions bindings, cross-references,
// and namespace containment. Seeds are derived from the iteration index and
// printed on failure, so any counterexample is deterministically reproducible.
//
// This replaces fixed-fixture property tests as the evidence for the bound: a
// single hand-built cycle proves the engine survives *that* graph; thousands of
// random graphs exercise the bound the theorem actually claims.
func TestClosurePropertiesOnRandomGraphs(t *testing.T) {
	const iterations = 3000
	maxClosure := 0 // guards against a vacuous generator (all closures = {target})
	for i := 0; i < iterations; i++ {
		seed := int64(0x52350000) + int64(i)
		objs, action := randomCluster(rand.New(rand.NewSource(seed)))
		st := NewScanState(objs)

		// Termination: run in a goroutine and fail (not hang) if it does not return.
		done := make(chan []Ref, 1)
		go func() { done <- Closure(st, action) }()
		var c []Ref
		select {
		case c = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("seed=%#x: Closure did not terminate on |R|=%d (verb=%s target=%s)",
				seed, len(objs), action.Verb, action.Target)
		}

		// |C| ≤ |R|.
		if len(c) > len(objs) {
			t.Fatalf("seed=%#x: |C|=%d exceeds |R|=%d", seed, len(c), len(objs))
		}
		if len(c) > maxClosure {
			maxClosure = len(c)
		}

		// C ⊆ R, and no duplicate members (the visited-set guard must dedup).
		known := make(map[string]bool, len(objs))
		for _, o := range objs {
			known[o.Ref.key()] = true
		}
		seen := make(map[string]bool, len(c))
		for _, r := range c {
			if !known[r.key()] {
				t.Fatalf("seed=%#x: closure member %s is not in inventory", seed, r)
			}
			if seen[r.key()] {
				t.Fatalf("seed=%#x: closure contains duplicate member %s", seed, r)
			}
			seen[r.key()] = true
		}

		// Safe must also not panic and must return a defined verdict on any input.
		if v := Safe(st, action, nil).Verdict; v < Allow || v > Block {
			t.Fatalf("seed=%#x: Safe returned out-of-range verdict %d", seed, int(v))
		}
	}

	// Non-vacuity: if every closure were just the target, the walk never followed
	// an edge and the bound was never exercised. Require at least one multi-member
	// closure across all iterations.
	if maxClosure < 2 {
		t.Fatalf("generator is vacuous: largest closure over %d iterations was %d (the walk never traversed a relation)", iterations, maxClosure)
	}
}

// randomCluster builds a small adversarial object set and a random action over
// it. It is intentionally promiscuous about edges — owners, selectors and
// cross-refs all point at random objects — so the generated graph routinely
// contains cycles, self-references and deep chains.
func randomCluster(rng *rand.Rand) ([]Object, Action) {
	kinds := []struct {
		group, version, kind string
	}{
		{"", "v1", "Pod"},
		{"apps", "v1", "Deployment"},
		{"apps", "v1", "ReplicaSet"},
		{"", "v1", "ConfigMap"},
		{"", "v1", "Secret"},
		{"", "v1", "Service"},
		{"networking.k8s.io", "v1", "NetworkPolicy"},
		{"", "v1", "Namespace"},
	}
	namespaces := []string{"ns1", "ns2"}
	labelKeys := []string{"app", "tier"}
	labelVals := []string{"a", "b", "c"}
	ops := []SelectorOperator{OpIn, OpNotIn, OpExists, OpDoesNotExist}

	n := 1 + rng.Intn(12)
	objs := make([]Object, n)
	for i := range objs {
		k := kinds[rng.Intn(len(kinds))]
		ns := namespaces[rng.Intn(len(namespaces))]
		name := fmt.Sprintf("o%d", i)
		if k.kind == "Namespace" {
			ns = "" // cluster-scoped; its Name is what containment keys off.
			name = namespaces[rng.Intn(len(namespaces))]
		}
		objs[i] = Object{
			Ref: Ref{
				GVK:       GVK{Group: k.group, Version: k.version, Kind: k.kind},
				Namespace: ns,
				Name:      name,
				UID:       fmt.Sprintf("uid:%d", i),
			},
			Labels:   randomLabels(rng, labelKeys, labelVals),
			Selector: randomSelector(rng, labelKeys, labelVals, ops),
		}
	}

	// Wire owners / cross-refs to random (possibly self) targets, creating cycles.
	for i := range objs {
		if rng.Float64() < 0.6 {
			tgt := objs[rng.Intn(n)]
			objs[i].Owners = append(objs[i].Owners, OwnerRef{
				Kind: tgt.Ref.GVK.Kind, Name: tgt.Ref.Name, UID: tgt.Ref.UID,
			})
		}
		if rng.Float64() < 0.4 {
			tgt := objs[rng.Intn(n)]
			objs[i].CrossRefs = append(objs[i].CrossRefs, CrossRef{
				Kind: RefVolume, Ref: tgt.Ref,
			})
		}
		if rng.Float64() < 0.2 {
			objs[i].Finalizers = append(objs[i].Finalizers, "example.com/guard")
		}
	}

	verbs := []Verb{Delete, Update, Patch, Scale, Restart}
	target := objs[rng.Intn(n)]
	action := Action{
		Verb:    verbs[rng.Intn(len(verbs))],
		Target:  target.Ref,
		Cascade: rng.Float64() < 0.7,
	}
	// For mutations, supply old/new payloads so selector/label/finalizer mutation
	// classes are exercised too.
	if action.Verb == Update || action.Verb == Patch {
		action.Old = &Object{
			Labels:   randomLabels(rng, labelKeys, labelVals),
			Selector: randomSelector(rng, labelKeys, labelVals, ops),
		}
		action.New = &Object{
			Labels:   randomLabels(rng, labelKeys, labelVals),
			Selector: randomSelector(rng, labelKeys, labelVals, ops),
		}
	}
	return objs, action
}

func randomLabels(rng *rand.Rand, keys, vals []string) map[string]string {
	if rng.Float64() < 0.2 {
		return nil
	}
	m := map[string]string{}
	for _, k := range keys {
		if rng.Float64() < 0.6 {
			m[k] = vals[rng.Intn(len(vals))]
		}
	}
	return m
}

func randomSelector(rng *rand.Rand, keys, vals []string, ops []SelectorOperator) LabelSelector {
	switch rng.Intn(4) {
	case 0:
		return LabelSelector{} // nil → binds nothing
	case 1:
		return LabelSelector{MatchLabels: map[string]string{}} // present-empty
	case 2:
		return LabelSelector{MatchLabels: randomLabels(rng, keys, vals)}
	default:
		var reqs []SelectorRequirement
		for _, k := range keys {
			if rng.Float64() < 0.5 {
				op := ops[rng.Intn(len(ops))]
				var v []string
				if op == OpIn || op == OpNotIn {
					v = []string{vals[rng.Intn(len(vals))]}
				}
				reqs = append(reqs, SelectorRequirement{Key: k, Operator: op, Values: v})
			}
		}
		return LabelSelector{MatchExpressions: reqs}
	}
}

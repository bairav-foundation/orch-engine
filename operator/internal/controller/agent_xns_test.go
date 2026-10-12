// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// stubClient implements just the Get/List calls resolveDeps makes. Hand-
// rolled instead of controller-runtime's fake client so the test adds no
// new module dependencies (the fake pulls in client-go/testing and would
// change go.mod, tripping ci.sh's tidy check). Any other method panics on
// the nil embedded interface, which would flag resolveDeps growing a call
// this stub doesn't model.
type stubClient struct {
	client.Client
	objs []client.Object
}

func (s *stubClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	for _, o := range s.objs {
		if o.GetNamespace() != key.Namespace || o.GetName() != key.Name {
			continue
		}
		switch dst := obj.(type) {
		case *orchv1alpha1.Model:
			if src, ok := o.(*orchv1alpha1.Model); ok {
				*dst = *src.DeepCopy()
				return nil
			}
		case *orchv1alpha1.Skill:
			if src, ok := o.(*orchv1alpha1.Skill); ok {
				*dst = *src.DeepCopy()
				return nil
			}
		case *orchv1alpha1.DLPPolicy:
			if src, ok := o.(*orchv1alpha1.DLPPolicy); ok {
				*dst = *src.DeepCopy()
				return nil
			}
		}
	}
	return apierrors.NewNotFound(schema.GroupResource{Group: "orch.bairavs.com", Resource: "stub"}, key.Name)
}

func (s *stubClient) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	if gl, ok := list.(*orchv1alpha1.AccessGrantList); ok {
		for _, o := range s.objs {
			if g, ok := o.(*orchv1alpha1.AccessGrant); ok && (lo.Namespace == "" || g.Namespace == lo.Namespace) {
				gl.Items = append(gl.Items, *g.DeepCopy())
			}
		}
	}
	return nil
}

func xnsReconciler(_ *testing.T, objs ...client.Object) *AgentReconciler {
	return &AgentReconciler{Client: &stubClient{objs: objs}}
}

func readyModel(ns, name string) *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	m.Status.Phase = orchv1alpha1.ModelPhaseReady
	m.Status.Endpoint = "http://" + name + "." + ns + ".svc.cluster.local:8080/v1"
	return m
}

func xnsAgent() *orchv1alpha1.Agent {
	a := &orchv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "bot"}}
	a.Spec.ModelRefs = []orchv1alpha1.LocalObjectReference{{Namespace: "team-a", Name: "qwen"}}
	return a
}

// No grant → the Agent waits, and the message carries the exact command.
func TestResolveDeps_CrossNamespaceWithoutGrant(t *testing.T) {
	r := xnsReconciler(t, readyModel("team-a", "qwen"))
	deps, err := r.resolveDeps(context.Background(), xnsAgent())
	if err != nil {
		t.Fatal(err)
	}
	if deps.allReady {
		t.Fatal("ungranted cross-namespace ref must not be Ready")
	}
	msg := waitingMessage(deps)
	if !strings.Contains(msg, "bairav grant team-b --model qwen -n team-a") {
		t.Errorf("message should carry the fix command, got %q", msg)
	}
	// The ungranted Agent must learn nothing about the target: no
	// missing/pending classification, which would leak existence.
	if len(deps.missingModels)+len(deps.pendingModels) != 0 {
		t.Errorf("ungranted ref leaked target state: missing=%v pending=%v", deps.missingModels, deps.pendingModels)
	}
}

// Matching grant → resolves, with ns/name display and the Model's endpoint.
func TestResolveDeps_CrossNamespaceWithGrant(t *testing.T) {
	grant := &orchv1alpha1.AccessGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "allow-team-b"},
		Spec: orchv1alpha1.AccessGrantSpec{
			From: []orchv1alpha1.AccessGrantFrom{{Namespace: "team-b"}},
			To:   []orchv1alpha1.AccessGrantTo{{Kind: orchv1alpha1.AccessGrantKindModel, Name: "qwen"}},
		},
	}
	r := xnsReconciler(t, readyModel("team-a", "qwen"), grant)
	deps, err := r.resolveDeps(context.Background(), xnsAgent())
	if err != nil {
		t.Fatal(err)
	}
	if !deps.allReady || len(deps.models) != 1 {
		t.Fatalf("granted ref should resolve; problems=%v", waitingMessage(deps))
	}
	m := deps.models[0]
	if m.name != "qwen" || m.display != "team-a/qwen" || !strings.Contains(m.endpoint, ".team-a.svc") {
		t.Errorf("resolved model = %+v", m)
	}
}

// A grant for a different consumer namespace doesn't count.
func TestResolveDeps_GrantForOtherNamespaceIgnored(t *testing.T) {
	grant := &orchv1alpha1.AccessGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "allow-team-c"},
		Spec: orchv1alpha1.AccessGrantSpec{
			From: []orchv1alpha1.AccessGrantFrom{{Namespace: "team-c"}},
			To:   []orchv1alpha1.AccessGrantTo{{Kind: orchv1alpha1.AccessGrantKindModel}},
		},
	}
	r := xnsReconciler(t, readyModel("team-a", "qwen"), grant)
	deps, err := r.resolveDeps(context.Background(), xnsAgent())
	if err != nil {
		t.Fatal(err)
	}
	if deps.allReady {
		t.Fatal("team-c's grant must not let team-b in")
	}
}

// Two refs with the same bare name would share one env var — rejected.
func TestResolveDeps_EnvCollision(t *testing.T) {
	a := xnsAgent()
	a.Spec.ModelRefs = append(a.Spec.ModelRefs, orchv1alpha1.LocalObjectReference{Name: "qwen"})
	r := xnsReconciler(t, readyModel("team-b", "qwen"))
	deps, err := r.resolveDeps(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if deps.allReady || !strings.Contains(waitingMessage(deps), "BAIRAV_MODEL_QWEN") {
		t.Errorf("expected env collision problem, got %q", waitingMessage(deps))
	}
}

// Cross-namespace dlpRef is rejected (a Pod can't mount another
// namespace's ConfigMap).
func TestResolveDeps_CrossNamespaceDLPRefRejected(t *testing.T) {
	a := &orchv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "bot"}}
	a.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Namespace: "team-a", Name: "p"}
	r := xnsReconciler(t)
	deps, err := r.resolveDeps(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if deps.allReady || !strings.Contains(deps.dlpProblem, "own namespace") {
		t.Errorf("expected same-namespace dlpRef problem, got %q", deps.dlpProblem)
	}
}

// Same-namespace refs never consult grants (zero behaviour change for
// every single-namespace Agent).
func TestResolveDeps_SameNamespaceNeedsNoGrant(t *testing.T) {
	a := &orchv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"}}
	a.Spec.ModelRefs = []orchv1alpha1.LocalObjectReference{{Name: "qwen"}}
	r := xnsReconciler(t, readyModel("team-a", "qwen"))
	deps, err := r.resolveDeps(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !deps.allReady || deps.models[0].display != "qwen" {
		t.Errorf("same-namespace ref should resolve with bare display; problems=%q", waitingMessage(deps))
	}
}

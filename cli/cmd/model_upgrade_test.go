// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCheckSmoke(t *testing.T) {
	var out strings.Builder
	if err := checkSmoke([]byte(`{"choices":[{"message":{"content":" OK "}}]}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"OK"`) {
		t.Errorf("want trimmed answer echoed, got %q", out.String())
	}
	for _, bad := range []string{`{"choices":[]}`, `not json`, `{"error":"model failed to load"}`} {
		if err := checkSmoke([]byte(bad), io.Discard); err == nil {
			t.Errorf("checkSmoke(%s) should fail", bad)
		}
	}
}

func TestSmokeHTTP(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer ok.Close()
	if err := smokeHTTP(context.Background(), ok.URL+"/v1", "hi", io.Discard); err != nil {
		t.Errorf("healthy model: %v", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "loading model", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	if err := smokeHTTP(context.Background(), broken.URL+"/v1", "hi", io.Discard); err == nil {
		t.Error("503 must fail the smoke test (and trigger rollback)")
	}
}

func TestReadUpgradeOptsStrategy(t *testing.T) {
	for in, want := range map[string]string{"": "", "surge": "Surge", "Recreate": "Recreate"} {
		c := &cobra.Command{}
		c.Flags().String("strategy", in, "")
		c.Flags().String("namespace", "default", "")
		o, err := readUpgradeOpts(c, "m")
		if err != nil || o.strategy != want {
			t.Errorf("strategy %q → %q, %v; want %q", in, o.strategy, err, want)
		}
		if !o.rollback || !o.smoke {
			t.Error("rollback and smoke must default on")
		}
	}
	c := &cobra.Command{}
	c.Flags().String("strategy", "bluegreen", "")
	if _, err := readUpgradeOpts(c, "m"); err == nil {
		t.Error("unknown strategy must be rejected")
	}
}

func TestModelLabel(t *testing.T) {
	if got := modelLabel("Qwen/x", ""); got != "Qwen/x" {
		t.Error(got)
	}
	if got := modelLabel("Qwen/x", "q4.gguf"); got != "Qwen/x (q4.gguf)" {
		t.Error(got)
	}
}

func TestPodProblem(t *testing.T) {
	waiting := func(reason string, restarts int32) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "m-abc", Namespace: "ns"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Image: "bairav/serving:laptop", RestartCount: restarts,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
			}}}}
	}
	// The Day 27 smoke failure: k3d node without the local image.
	if p := podProblem([]corev1.Pod{waiting("ImagePullBackOff", 0)}); !p.fatal || !strings.Contains(p.msg, "k3d image import bairav/serving:laptop") {
		t.Errorf("image pull: %+v", p)
	}
	if p := podProblem([]corev1.Pod{waiting("CrashLoopBackOff", 1)}); p.fatal {
		t.Error("one restart is not yet a crash loop")
	}
	if p := podProblem([]corev1.Pod{waiting("CrashLoopBackOff", 3)}); !p.fatal {
		t.Error("3 restarts should be fatal")
	}
	if p := podProblem([]corev1.Pod{waiting("ContainerCreating", 0)}); p.fatal || p.unschedulable {
		t.Error("ContainerCreating is normal progress")
	}
	unsched := corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes: Insufficient memory"}}}}
	if p := podProblem([]corev1.Pod{unsched}); !p.unschedulable || !strings.Contains(p.msg, "--strategy recreate") {
		t.Errorf("unschedulable: %+v", p)
	}
}

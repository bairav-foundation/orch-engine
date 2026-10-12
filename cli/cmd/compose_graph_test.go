// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTargetCmd builds a throwaway command carrying the flags
// resolveCmdTarget inspects, then parses args into it.
func newTargetCmd(t *testing.T, args ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	c := &cobra.Command{Use: "x"}
	c.Flags().String("target", "", "")
	c.Flags().String("namespace", "default", "")
	c.Flags().Int("host-port", 0, "")
	c.Flags().String("dlp-policy", "", "")
	c.Flags().StringSlice("dlp-pack", nil, "")
	var stderr bytes.Buffer
	c.SetErr(&stderr)
	if err := c.Flags().Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return c, &stderr
}

func TestResolveCmdTarget_DefaultIsDockerWithBanner(t *testing.T) {
	c, stderr := newTargetCmd(t)
	got, err := resolveCmdTarget(c)
	if err != nil || got != "docker" {
		t.Fatalf("got (%q, %v), want docker", got, err)
	}
	if !strings.Contains(stderr.String(), "defaulting to docker") {
		t.Errorf("missing default banner, stderr=%q", stderr.String())
	}
}

func TestResolveCmdTarget_ExplicitK8sNoBanner(t *testing.T) {
	c, stderr := newTargetCmd(t, "--target", "k8s", "--namespace", "prod")
	got, err := resolveCmdTarget(c)
	if err != nil || got != "k8s" {
		t.Fatalf("got (%q, %v), want k8s", got, err)
	}
	if stderr.Len() != 0 {
		t.Errorf("explicit --target must not print the banner, got %q", stderr.String())
	}
}

func TestResolveCmdTarget_WrongTargetFlagsRejected(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--namespace", "prod"}, "--namespace doesn't apply to --target docker"},
		{[]string{"--dlp-policy", "p"}, "--dlp-policy doesn't apply to --target docker"},
		{[]string{"--target", "k8s", "--host-port", "9000"}, "--host-port doesn't apply to --target k8s"},
		{[]string{"--target", "k8s", "--dlp-pack", "india"}, "--dlp-pack doesn't apply to --target k8s"},
		{[]string{"--target", "nomad"}, "unknown --target"},
	}
	for _, tc := range cases {
		c, _ := newTargetCmd(t, tc.args...)
		_, err := resolveCmdTarget(c)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("args %v: err=%v, want containing %q", tc.args, err, tc.wantErr)
		}
	}
}

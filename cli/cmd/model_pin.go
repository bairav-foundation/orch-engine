// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// Model pinning: `model deploy` and `model upgrade` resolve the Hugging
// Face revision to the exact commit and look up the SHA-256 of the file to
// serve, and write both into the deployment. The serving container checks
// the hash before it starts, so the model can't change under you: not by a
// push to the repo, not by a tampered cache. --no-pin keeps the old
// behaviour (follow the branch, no hash).

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// addPinFlags adds --revision and --no-pin.
func addPinFlags(c *cobra.Command) {
	c.Flags().String("revision", "main",
		"Hugging Face branch, tag or commit to serve; resolved to a commit and pinned unless --no-pin")
	c.Flags().Bool("no-pin", false,
		"don't pin the commit and file SHA-256 (follow the branch on every Pod start, no integrity check)")
}

// pinLocalRef fills ref.Revision / ref.SHA256 (and ref.File, when empty)
// for a local model. backend vllm gets the commit only: it serves a whole
// safetensors repo, not one file.
func pinLocalRef(ctx context.Context, cmd *cobra.Command, ref *modelRef, backend string) error {
	if ref.isCloud() {
		return nil
	}
	revision, _ := cmd.Flags().GetString("revision")
	if noPin, _ := cmd.Flags().GetBool("no-pin"); noPin {
		ref.Revision, ref.SHA256 = orDefault(revision, "main"), ""
		return nil
	}
	c := hf.NewClient()
	if backend == orchv1alpha1.ModelBackendVLLM {
		commit, err := c.ResolveCommit(ctx, ref.Repo, revision)
		if err != nil {
			return pinErr(err)
		}
		ref.Revision, ref.SHA256 = commit, ""
		printPin(cmd.OutOrStdout(), ref)
		return nil
	}
	p, err := c.Pin(ctx, ref.Repo, revision, ref.File, smallestGGUF)
	if err != nil {
		return pinErr(err)
	}
	ref.File, ref.Revision, ref.SHA256 = p.File, p.Commit, p.SHA256
	printPin(cmd.OutOrStdout(), ref)
	return nil
}

func pinErr(err error) error {
	return fmt.Errorf("%w\n(pass --no-pin to deploy without pinning the model, e.g. when Hugging Face isn't reachable from here)", err)
}

func printPin(out io.Writer, ref *modelRef) {
	if ref.SHA256 != "" {
		fmt.Fprintf(out, "Pinned:    %s @ %s, %s sha256 %s…\n", ref.Repo, short(ref.Revision, 12), ref.File, short(ref.SHA256, 16))
		return
	}
	fmt.Fprintf(out, "Pinned:    %s @ %s\n", ref.Repo, short(ref.Revision, 12))
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

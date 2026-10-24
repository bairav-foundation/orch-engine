// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/egress"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// addEgressFlags adds --egress / --egress-allow to agent and skill deploy.
func addEgressFlags(c *cobra.Command) {
	c.Flags().String("egress", "",
		"k8s target: open | restricted. restricted = the Pods may reach only DNS, their own Models/Skills and --egress-allow (NetworkPolicy). Redeploying without it keeps the current setting")
	c.Flags().StringArray("egress-allow", nil,
		"k8s target, with --egress restricted: allowed destination CIDR[:port,port] (repeatable), e.g. 203.0.113.7/32:443")
}

// egressFlagsSet reports whether the user passed either egress flag.
func egressFlagsSet(c *cobra.Command) bool {
	return c.Flags().Changed("egress") || c.Flags().Changed("egress-allow")
}

// egressFromFlags turns the flags into a spec. nil = open (no policy).
func egressFromFlags(c *cobra.Command) (*orchv1alpha1.EgressSpec, error) {
	modeFlag, _ := c.Flags().GetString("egress")
	allow, _ := c.Flags().GetStringArray("egress-allow")
	mode, err := egress.ParseMode(strings.TrimSpace(modeFlag))
	if err != nil {
		return nil, err
	}
	if len(allow) > 0 && mode != "restricted" {
		return nil, fmt.Errorf("--egress-allow only makes sense with --egress restricted")
	}
	if mode == "open" {
		return nil, nil
	}
	spec := &orchv1alpha1.EgressSpec{Mode: orchv1alpha1.EgressRestricted}
	for _, a := range allow {
		r, err := egress.ParseAllow(a)
		if err != nil {
			return nil, err
		}
		spec.Allow = append(spec.Allow, orchv1alpha1.EgressRule{CIDR: r.CIDR, Ports: r.Ports})
	}
	return spec, nil
}

// printEgress writes the Egress: line of the deploy summary.
func printEgress(out io.Writer, spec *orchv1alpha1.EgressSpec) {
	if !spec.Restricted() {
		return
	}
	var allow []string
	for _, a := range spec.Allow {
		s := a.CIDR
		if len(a.Ports) > 0 {
			ps := make([]string, len(a.Ports))
			for i, p := range a.Ports {
				ps[i] = fmt.Sprint(p)
			}
			s += ":" + strings.Join(ps, ",")
		}
		allow = append(allow, s)
	}
	extra := "nothing else"
	if len(allow) > 0 {
		extra = strings.Join(allow, ", ")
	}
	fmt.Fprintf(out, "Egress:     restricted (DNS, own dependencies, %s)\n", extra)
}

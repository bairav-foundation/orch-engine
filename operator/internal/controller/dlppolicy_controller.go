// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// DLPPolicyReconciler validates a DLPPolicy CR and, when the policy
// declares inline customRules, projects them into a ConfigMap the DLP
// sidecars mount (Day 19). It never schedules a workload itself — Agents
// reference the policy via spec.dlpRef and the AgentReconciler injects
// the sidecars.
//
// A policy is Ready when:
//   - At least one Pack OR one CustomRule is declared.
//   - Every named Pack is on the known list.
//   - Every CustomRule has an uppercase label, a compiling regex, a known
//     checksum (or none) and a known action (or none).
//   - No CustomRule label collides with a built-in detector, an enabled
//     pack's rule, or another CustomRule. The sidecar's CLI loader rejects
//     these at startup — catching them here turns a CrashLoopBackOff into
//     a readable Condition.
//   - The customRules ConfigMap (if any) has been applied.
//
// Anything else → phase Invalid + Validated=False naming the problem.
type DLPPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orch.bairavs.com,resources=dlppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=dlppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// packLabels mirrors the rule labels inside each embedded pack in
// cli/internal/dlp/packs/*.yaml. Mirrored (not imported) so the operator
// module doesn't depend on the CLI module. Used for three things: the
// known-pack whitelist, the RuleCount status column, and pack-vs-custom
// label collision checks. UPDATE THIS when a pack gains or loses rules.
var packLabels = map[string][]string{
	"global-safe": {"AADHAAR", "UK_NHS"},
	"india":       {"PAN", "GSTIN", "INDIA_MOBILE", "INDIA_LANDLINE"},
	"uk":          {"UK_NINO"},
}

// builtinLabels mirrors cli/internal/dlp/policy.go's reserved set — the
// always-on detectors a custom rule may not shadow.
var builtinLabels = map[string]bool{
	"EMAIL":          true,
	"SSN":            true,
	"CREDIT_CARD":    true,
	"PHONE_US":       true,
	"AWS_ACCESS_KEY": true,
}

// knownChecksums mirrors cli/internal/dlp/checksums.go's registry.
var knownChecksums = map[string]bool{
	"":         true, // empty = no checksum
	"luhn":     true,
	"mod10":    true,
	"verhoeff": true,
	"mod11":    true,
}

// labelRe matches the CLI's own label regex.
var labelRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// DLPPolicyFileName is the key inside the projected ConfigMap and the
// file name the sidecar reads.
const DLPPolicyFileName = "policy.yaml"

// DLPPolicyMountDir is where the sidecar sees the projected ConfigMap.
const DLPPolicyMountDir = "/etc/bairav/policy"

// DLPPolicyConfigMapName is the name of the ConfigMap holding a policy's
// projected customRules. Deterministic so the AgentReconciler can mount
// it without an extra lookup.
func DLPPolicyConfigMapName(policyName string) string {
	return "dlppolicy-" + policyName
}

// Reconcile validates, projects customRules, and patches status.
func (r *DLPPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("dlppolicy", req.NamespacedName)

	var p orchv1alpha1.DLPPolicy
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // owner ref cascades the ConfigMap
		}
		return ctrl.Result{}, fmt.Errorf("get dlppolicy: %w", err)
	}

	newStatus := r.validate(&p)

	// Only project when the spec is valid — an Invalid policy must never
	// reach a sidecar, and Agents won't bind to it anyway.
	if newStatus.Phase == orchv1alpha1.DLPPolicyPhaseReady {
		if err := r.reconcileConfigMap(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}

	if dlpPolicyStatusChanged(p.Status, newStatus) {
		base := p.DeepCopy()
		p.Status = newStatus
		if err := r.Status().Patch(ctx, &p, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch dlppolicy status: %w", err)
		}
	}

	logger.V(1).Info("reconciled", "phase", newStatus.Phase, "rules", newStatus.RuleCount)
	return ctrl.Result{}, nil
}

// reconcileConfigMap creates/updates the projected ConfigMap when the
// policy has customRules, and deletes a stale one when it doesn't.
func (r *DLPPolicyReconciler) reconcileConfigMap(ctx context.Context, p *orchv1alpha1.DLPPolicy) error {
	key := types.NamespacedName{Namespace: p.Namespace, Name: DLPPolicyConfigMapName(p.Name)}

	if len(p.Spec.CustomRules) == 0 {
		var stale corev1.ConfigMap
		if err := r.Get(ctx, key, &stale); err == nil {
			if derr := r.Delete(ctx, &stale); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("delete stale policy configmap: %w", derr)
			}
		}
		return nil
	}

	body, err := RenderPolicyFile(p.Spec.CustomRules)
	if err != nil {
		return fmt.Errorf("render policy file: %w", err)
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "bairav-operator",
				"orch.bairavs.com/dlppolicy":   p.Name,
			},
		},
		Data: map[string]string{DLPPolicyFileName: string(body)},
	}
	if err := ctrl.SetControllerReference(p, desired, r.Scheme); err != nil {
		return fmt.Errorf("set configmap owner: %w", err)
	}

	var existing corev1.ConfigMap
	err = r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get policy configmap: %w", err)
	}
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	existing.OwnerReferences = desired.OwnerReferences
	return r.Update(ctx, &existing)
}

// policyFile / policyRule mirror cli/internal/dlp.Policy / Rule. We emit
// JSON — valid YAML, so the CLI's yaml.v3 loader reads it unchanged —
// which avoids adding a YAML dependency to the operator module and makes
// quoting of arbitrary regexes bulletproof.
type policyFile struct {
	Version string       `json:"version"`
	Rules   []policyRule `json:"rules"`
}

type policyRule struct {
	Label    string `json:"label"`
	Pattern  string `json:"pattern"`
	Action   string `json:"action,omitempty"`
	Checksum string `json:"checksum,omitempty"`
}

// RenderPolicyFile converts CR-shaped customRules into the CLI's policy
// v1 file format. CR action "audit" maps to the CLI's "audit_only".
func RenderPolicyFile(rules []orchv1alpha1.DLPCustomRule) ([]byte, error) {
	f := policyFile{Version: "v1", Rules: make([]policyRule, 0, len(rules))}
	for _, cr := range rules {
		action := string(cr.Action)
		if cr.Action == orchv1alpha1.DLPActionAudit {
			action = "audit_only"
		}
		f.Rules = append(f.Rules, policyRule{
			Label:    cr.Label,
			Pattern:  cr.Regex,
			Action:   action,
			Checksum: strings.ToLower(cr.Checksum),
		})
	}
	return json.MarshalIndent(f, "", "  ")
}

// DLPPolicyHash is a short, stable fingerprint of everything in a policy
// spec that affects sidecar behaviour. The AgentReconciler stamps it on
// the Pod template so ANY policy edit (including customRules, which live
// in a ConfigMap and wouldn't otherwise restart the Pod) rolls the Agent.
func DLPPolicyHash(spec orchv1alpha1.DLPPolicySpec) string {
	b, _ := json.Marshal(spec) // spec is plain data; Marshal can't fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// validate runs every check and returns the new status.
func (r *DLPPolicyReconciler) validate(p *orchv1alpha1.DLPPolicy) orchv1alpha1.DLPPolicyStatus {
	out := *p.Status.DeepCopy()
	out.ObservedGeneration = p.Generation

	var problems []string

	if len(p.Spec.Packs) == 0 && len(p.Spec.CustomRules) == 0 {
		problems = append(problems,
			"policy has no packs and no customRules — at least one is required")
	}

	// Packs: known names; collect the labels they claim.
	var unknownPacks []string
	packOwner := map[string]string{} // label → pack that defines it
	ruleCount := int32(0)
	for _, name := range p.Spec.Packs {
		labels, ok := packLabels[name]
		if !ok {
			unknownPacks = append(unknownPacks, name)
			continue
		}
		ruleCount += int32(len(labels))
		for _, l := range labels {
			packOwner[l] = name
		}
	}
	if len(unknownPacks) > 0 {
		problems = append(problems,
			fmt.Sprintf("unknown pack(s): %s (known: %s)",
				strings.Join(unknownPacks, ","), knownPackList()))
	}

	// Custom rules.
	seen := map[string]bool{}
	for i, cr := range p.Spec.CustomRules {
		if !labelRe.MatchString(cr.Label) {
			problems = append(problems,
				fmt.Sprintf("customRules[%d].label %q must match ^[A-Z][A-Z0-9_]*$", i, cr.Label))
		}
		switch {
		case builtinLabels[cr.Label]:
			problems = append(problems,
				fmt.Sprintf("customRules[%d].label %q collides with a built-in detector", i, cr.Label))
		case packOwner[cr.Label] != "":
			problems = append(problems,
				fmt.Sprintf("customRules[%d].label %q is already defined by pack %q", i, cr.Label, packOwner[cr.Label]))
		case seen[cr.Label]:
			problems = append(problems,
				fmt.Sprintf("customRules[%d].label %q is defined more than once", i, cr.Label))
		}
		seen[cr.Label] = true
		if _, err := regexp.Compile(cr.Regex); err != nil {
			problems = append(problems,
				fmt.Sprintf("customRules[%d].regex does not compile: %v", i, err))
		}
		if !knownChecksums[strings.ToLower(cr.Checksum)] {
			problems = append(problems,
				fmt.Sprintf("customRules[%d].checksum %q unknown (known: luhn, mod10, verhoeff, mod11)", i, cr.Checksum))
		}
		switch cr.Action {
		case "", orchv1alpha1.DLPActionAudit, orchv1alpha1.DLPActionRedact, orchv1alpha1.DLPActionBlock:
		default:
			problems = append(problems,
				fmt.Sprintf("customRules[%d].action %q must be audit / redact / block", i, cr.Action))
		}
		ruleCount++
	}
	out.RuleCount = ruleCount

	if len(problems) > 0 {
		out.Phase = orchv1alpha1.DLPPolicyPhaseInvalid
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:               "Validated",
			Status:             metav1.ConditionFalse,
			Reason:             "SpecInvalid",
			Message:            strings.Join(problems, "; "),
			ObservedGeneration: p.Generation,
		})
		return out
	}

	out.Phase = orchv1alpha1.DLPPolicyPhaseReady
	meta.SetStatusCondition(&out.Conditions, metav1.Condition{
		Type:               "Validated",
		Status:             metav1.ConditionTrue,
		Reason:             "SpecValid",
		Message:            fmt.Sprintf("packs=%d customRules=%d", len(p.Spec.Packs), len(p.Spec.CustomRules)),
		ObservedGeneration: p.Generation,
	})
	return out
}

func knownPackList() string {
	names := make([]string, 0, len(packLabels))
	for k := range packLabels {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic Condition message
	return strings.Join(names, ",")
}

// dlpPolicyStatusChanged — cheap comparison to skip no-op status patches.
func dlpPolicyStatusChanged(a, b orchv1alpha1.DLPPolicyStatus) bool {
	if a.Phase != b.Phase ||
		a.ObservedGeneration != b.ObservedGeneration ||
		a.RuleCount != b.RuleCount ||
		len(a.Conditions) != len(b.Conditions) {
		return true
	}
	for i := range a.Conditions {
		if a.Conditions[i].Type != b.Conditions[i].Type ||
			a.Conditions[i].Status != b.Conditions[i].Status ||
			a.Conditions[i].Reason != b.Conditions[i].Reason ||
			a.Conditions[i].Message != b.Conditions[i].Message {
			return true
		}
	}
	return false
}

// SetupWithManager — Owns the projected ConfigMap so a manual edit or
// delete of it is reconciled back to the spec.
func (r *DLPPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchv1alpha1.DLPPolicy{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

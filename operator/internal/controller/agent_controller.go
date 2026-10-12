// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// AgentReconciler reconciles an Agent object into a Deployment + Service
// pair, resolving referenced Models and Skills at reconcile time and
// injecting their endpoints as environment variables.
//
// Same operator pattern as ModelReconciler and SkillReconciler. The
// interesting difference is dependency resolution: an Agent CR names
// Model + Skill CRs it needs; the reconciler Get()s each, and if any
// referenced Model isn't Ready yet, the Agent's phase becomes
// WaitingForDeps and reconcile requeues in 5 seconds. The Agent's Pod
// doesn't roll until every dependency has a real endpoint, so the
// container never sees a half-wired environment.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orch.bairavs.com,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=dlppolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=accessgrants,verbs=get;list;watch

// Reconcile is the controller's main loop.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agent", req.NamespacedName)

	var a orchv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get agent: %w", err)
	}

	// 1. Resolve dependencies.
	deps, err := r.resolveDeps(ctx, &a)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2. If any dependency isn't Ready, mark WaitingForDeps and requeue.
	// We don't create the Deployment yet — better to have zero replicas
	// while waiting than a running Pod pointed at missing endpoints.
	if !deps.allReady {
		if err := r.setPhaseWaiting(ctx, &a, deps); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 3. Reconcile Deployment + Service now that deps are ready.
	desiredDeploy := r.buildDeployment(&a, deps)
	if err := ctrl.SetControllerReference(&a, desiredDeploy, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set deploy owner: %w", err)
	}
	if err := r.applyAgentDeployment(ctx, desiredDeploy); err != nil {
		return ctrl.Result{}, err
	}

	desiredSvc := r.buildService(&a)
	if err := ctrl.SetControllerReference(&a, desiredSvc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set svc owner: %w", err)
	}
	if err := r.applyAgentService(ctx, desiredSvc); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Compute status from observed Deployment.
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Name}, &deploy); err == nil {
		newStatus := r.computeStatus(&a, &deploy, desiredSvc, deps)
		if agentStatusChanged(a.Status, newStatus) {
			// Patch not Update — same concurrency-race reasoning as
			// Model and Skill.
			base := a.DeepCopy()
			a.Status = newStatus
			if err := r.Status().Patch(ctx, &a, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
			}
		}
	}

	logger.V(1).Info("reconciled", "phase", a.Status.Phase,
		"models", deps.modelNames(), "skills", deps.skillNames())
	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Dependency resolution
// -----------------------------------------------------------------------------

// resolved is the outcome of dependency resolution. Bound entries are
// Ready and have an endpoint; Pending entries were found but aren't
// Ready yet; Missing entries don't exist as CRs. DLP is included so
// spec.dlpRef participates in the same WaitingForDeps gate.
type resolved struct {
	models        []resolvedModel
	skills        []resolvedSkill
	dlp           *resolvedDLPPolicy // nil when Agent has no dlpRef
	missingModels []string
	missingSkills []string
	pendingModels []string
	pendingSkills []string
	// dlpProblem is the single problem preventing DLP binding — the
	// ref name plus what's wrong ("missing", "pending", "invalid").
	// Empty string when DLP is either not referenced or bound cleanly.
	dlpProblem string
	// refProblems are cross-namespace problems: a ref with no
	// matching AccessGrant, or two refs that would collide on one env var.
	// Each message carries the exact fix.
	refProblems []string
}

// resolvedModel / resolvedSkill: name is the bare ref name (drives env-var
// and container names); display is "ns/name" for cross-namespace refs and
// plain name otherwise (drives status + logs).
type resolvedModel struct {
	name     string
	display  string
	endpoint string
}

type resolvedSkill struct {
	name      string
	display   string
	endpoint  string
	transport orchv1alpha1.SkillTransport
}

// refNamespace is the effective namespace of a ref.
func refNamespace(a *orchv1alpha1.Agent, ref orchv1alpha1.LocalObjectReference) string {
	if ref.Namespace == "" {
		return a.Namespace
	}
	return ref.Namespace
}

// refDisplay renders a ref for status/messages: bare name when local.
func refDisplay(a *orchv1alpha1.Agent, ref orchv1alpha1.LocalObjectReference) string {
	if ns := refNamespace(a, ref); ns != a.Namespace {
		return ns + "/" + ref.Name
	}
	return ref.Name
}

// grantChecker answers "may namespace X use kind/name in namespace Y?",
// listing each target namespace's AccessGrants at most once per reconcile.
type grantChecker struct {
	r     *AgentReconciler
	cache map[string][]orchv1alpha1.AccessGrant
}

func (g *grantChecker) allowed(ctx context.Context, targetNS, fromNS string, kind orchv1alpha1.AccessGrantKind, name string) (bool, error) {
	if targetNS == fromNS {
		return true, nil // same-namespace refs never need a grant
	}
	grants, ok := g.cache[targetNS]
	if !ok {
		var list orchv1alpha1.AccessGrantList
		if err := g.r.List(ctx, &list, client.InNamespace(targetNS)); err != nil {
			return false, fmt.Errorf("list accessgrants in %s: %w", targetNS, err)
		}
		grants = list.Items
		g.cache[targetNS] = grants
	}
	for i := range grants {
		if grants[i].Allows(fromNS, kind, name) {
			return true, nil
		}
	}
	return false, nil
}

// grantHint is the actionable message for an ungranted ref.
func grantHint(fromNS, targetNS string, kind orchv1alpha1.AccessGrantKind, name string) string {
	flag := "--model"
	if kind == orchv1alpha1.AccessGrantKindSkill {
		flag = "--skill"
	}
	return fmt.Sprintf("no AccessGrant in %s lets %s use %s %s (owner runs: bairav grant %s %s %s -n %s)",
		targetNS, fromNS, kind, name, fromNS, flag, name, targetNS)
}

// resolvedDLPPolicy captures everything the Agent's build step needs
// from the referenced DLPPolicy CR. Kept as a snapshot (not a *CR
// pointer) so the sidecar-injection code is easy to unit-test with
// hand-built fixtures.
type resolvedDLPPolicy struct {
	name          string
	packs         []string
	defaultAction orchv1alpha1.DLPAction
	direction     orchv1alpha1.DLPDirection
	auditPath     string
	// customRulesCount > 0 means the DLPPolicyReconciler has projected
	// the rules into ConfigMap DLPPolicyConfigMapName(name); the sidecar
	// mounts it and reads it via BAIRAV_DLP_POLICY.
	customRulesCount int
	// hash fingerprints the policy spec; stamped on the Pod template so a
	// policy edit rolls the Agent (ConfigMap edits alone wouldn't).
	hash string
}

// modelNames / skillNames return sorted lists of bound (Ready) deps —
// used by the log line + audit trail.
func (r resolved) modelNames() []string {
	out := make([]string, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m.display)
	}
	sort.Strings(out)
	return out
}

func (r resolved) skillNames() []string {
	out := make([]string, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s.display)
	}
	sort.Strings(out)
	return out
}

// envCollisions returns problems for refs whose bare names map to the same
// env var (e.g. team-a/qwen and team-b/qwen both → BAIRAV_MODEL_QWEN_*).
func envCollisions(kind string, a *orchv1alpha1.Agent, refs []orchv1alpha1.LocalObjectReference) []string {
	seen := map[string]string{}
	var out []string
	for _, ref := range refs {
		k := envKey(ref.Name)
		if prev, dup := seen[k]; dup {
			out = append(out, fmt.Sprintf("%s refs %s and %s both map to env BAIRAV_%s_%s_* — references must have distinct names",
				kind, prev, refDisplay(a, ref), strings.ToUpper(kind), k))
			continue
		}
		seen[k] = refDisplay(a, ref)
	}
	return out
}

// resolveDeps walks the ModelRefs + SkillRefs, Get()s each, and
// classifies them. Cross-namespace refs must be covered by an
// AccessGrant in the target namespace; we check the grant BEFORE reading
// the target so an ungranted Agent learns nothing about it (not even
// whether it exists).
func (r *AgentReconciler) resolveDeps(ctx context.Context, a *orchv1alpha1.Agent) (*resolvedWithFlag, error) {
	res := resolvedWithFlag{}
	gc := &grantChecker{r: r, cache: map[string][]orchv1alpha1.AccessGrant{}}
	res.refProblems = append(res.refProblems, envCollisions("model", a, a.Spec.ModelRefs)...)
	res.refProblems = append(res.refProblems, envCollisions("skill", a, a.Spec.SkillRefs)...)

	for _, ref := range a.Spec.ModelRefs {
		ns, disp := refNamespace(a, ref), refDisplay(a, ref)
		ok, err := gc.allowed(ctx, ns, a.Namespace, orchv1alpha1.AccessGrantKindModel, ref.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.refProblems = append(res.refProblems, grantHint(a.Namespace, ns, orchv1alpha1.AccessGrantKindModel, ref.Name))
			continue
		}
		var m orchv1alpha1.Model
		err = r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &m)
		if apierrors.IsNotFound(err) {
			res.missingModels = append(res.missingModels, disp)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve model %q: %w", disp, err)
		}
		if m.Status.Phase != orchv1alpha1.ModelPhaseReady || m.Status.Endpoint == "" {
			res.pendingModels = append(res.pendingModels, disp)
			continue
		}
		res.models = append(res.models, resolvedModel{name: ref.Name, display: disp, endpoint: m.Status.Endpoint})
	}
	for _, ref := range a.Spec.SkillRefs {
		ns, disp := refNamespace(a, ref), refDisplay(a, ref)
		ok, err := gc.allowed(ctx, ns, a.Namespace, orchv1alpha1.AccessGrantKindSkill, ref.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.refProblems = append(res.refProblems, grantHint(a.Namespace, ns, orchv1alpha1.AccessGrantKindSkill, ref.Name))
			continue
		}
		var s orchv1alpha1.Skill
		err = r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &s)
		if apierrors.IsNotFound(err) {
			res.missingSkills = append(res.missingSkills, disp)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q: %w", disp, err)
		}
		if s.Status.Phase != orchv1alpha1.SkillPhaseReady || s.Status.Endpoint == "" {
			res.pendingSkills = append(res.pendingSkills, disp)
			continue
		}
		res.skills = append(res.skills, resolvedSkill{
			name:      ref.Name,
			display:   disp,
			endpoint:  s.Status.Endpoint,
			transport: s.Spec.Transport,
		})
	}
	// dlpRef must stay in the Agent's namespace: the sidecar mounts the
	// policy's ConfigMap, and Pods can't mount ConfigMaps cross-namespace.
	if a.Spec.DLPRef != nil && a.Spec.DLPRef.Namespace != "" && a.Spec.DLPRef.Namespace != a.Namespace {
		res.dlpProblem = fmt.Sprintf("dlpRef %s/%s: DLPPolicy must be in the Agent's own namespace (%s)",
			a.Spec.DLPRef.Namespace, a.Spec.DLPRef.Name, a.Namespace)
	} else if a.Spec.DLPRef != nil {
		// Resolve the optional DLPPolicy ref. When set, the policy must be
		// Ready before the Agent is allowed to roll — same rule as Model +
		// Skill refs, so the WaitingForDeps semantics stay uniform.
		var p orchv1alpha1.DLPPolicy
		err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.DLPRef.Name}, &p)
		switch {
		case apierrors.IsNotFound(err):
			res.dlpProblem = fmt.Sprintf("missing DLPPolicy: %s", a.Spec.DLPRef.Name)
		case err != nil:
			return nil, fmt.Errorf("resolve dlppolicy %q: %w", a.Spec.DLPRef.Name, err)
		case p.Status.Phase == orchv1alpha1.DLPPolicyPhaseInvalid:
			res.dlpProblem = fmt.Sprintf("invalid DLPPolicy: %s (see its Validated condition)", a.Spec.DLPRef.Name)
		case p.Status.Phase != orchv1alpha1.DLPPolicyPhaseReady:
			res.dlpProblem = fmt.Sprintf("pending DLPPolicy: %s", a.Spec.DLPRef.Name)
		default:
			// Snapshot the policy fields the build step needs. Defaults
			// applied here (not in buildDeployment) so downstream code
			// sees a normalised value regardless of whether the user
			// omitted an optional field in their CR.
			action := p.Spec.DefaultAction
			if action == "" {
				action = orchv1alpha1.DLPActionRedact
			}
			dir := p.Spec.Direction
			if dir == "" {
				dir = orchv1alpha1.DLPDirectionBoth
			}
			audit := p.Spec.AuditPath
			if audit == "" {
				audit = "-"
			}
			res.dlp = &resolvedDLPPolicy{
				name:             p.Name,
				packs:            append([]string(nil), p.Spec.Packs...),
				defaultAction:    action,
				direction:        dir,
				auditPath:        audit,
				customRulesCount: len(p.Spec.CustomRules),
				hash:             DLPPolicyHash(p.Spec),
			}
		}
	}

	res.allReady = len(res.missingModels) == 0 &&
		len(res.missingSkills) == 0 &&
		len(res.pendingModels) == 0 &&
		len(res.pendingSkills) == 0 &&
		len(res.refProblems) == 0 &&
		res.dlpProblem == ""
	return &res, nil
}

// resolvedWithFlag is `resolved` plus a bool. Split into a separate type
// so `allReady` is read like a field in the caller (deps.allReady) — makes
// the branch condition read naturally.
type resolvedWithFlag struct {
	resolved
	allReady bool
}

// -----------------------------------------------------------------------------
// build helpers
// -----------------------------------------------------------------------------

// buildDeployment constructs the desired Deployment for an Agent,
// injecting BAIRAV_MODEL_*_ENDPOINT and BAIRAV_SKILL_*_ENDPOINT env vars
// for every resolved dependency.
func (r *AgentReconciler) buildDeployment(a *orchv1alpha1.Agent, deps *resolvedWithFlag) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "bairav-agent",
		"app.kubernetes.io/instance":   a.Name,
		"app.kubernetes.io/managed-by": "bairav-operator",
		"orch.bairavs.com/agent":       a.Name,
	}
	port := a.Spec.Port
	if port == 0 {
		port = 8000
	}
	healthPath := a.Spec.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}

	// Compute the effective Model endpoints. When a DLPPolicy is bound,
	// each Model's endpoint is rewritten to point at a per-Model DLP
	// sidecar listening on localhost. Otherwise env vars carry the raw
	// Service DNS as before.
	modelEndpoints, sidecars := computeModelEndpointsAndSidecars(a, deps)

	// Start with the user-supplied Env, then append operator-injected
	// endpoint vars. Ordering matters only for humans reading `kubectl
	// describe pod` — Go env resolution is by name, not position.
	env := append([]corev1.EnvVar{}, a.Spec.Env...)
	for _, m := range deps.models {
		env = append(env, corev1.EnvVar{
			Name:  fmt.Sprintf("BAIRAV_MODEL_%s_ENDPOINT", envKey(m.name)),
			Value: modelEndpoints[m.name],
		})
	}
	for _, s := range deps.skills {
		env = append(env, corev1.EnvVar{
			Name:  fmt.Sprintf("BAIRAV_SKILL_%s_ENDPOINT", envKey(s.name)),
			Value: s.endpoint,
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BAIRAV_SKILL_%s_TRANSPORT", envKey(s.name)),
			Value: string(s.transport),
		})
	}

	resources := a.Spec.Resources
	if resources.Requests == nil && resources.Limits == nil {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
	}

	agentContainer := corev1.Container{
		Name:      "agent",
		Image:     a.Spec.Image,
		Env:       env,
		Resources: resources,
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: port,
			Protocol:      corev1.ProtocolTCP,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       5,
			FailureThreshold:    12,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       15,
			FailureThreshold:    3,
		},
	}

	// Sidecars is the list of DLP proxy containers (one per Model when
	// spec.dlpRef is set). Empty slice when DLP is not enabled — the
	// Pod spec then looks identical to the no-DLP shape.
	containers := append([]corev1.Container{agentContainer}, sidecars...)

	// DLP extras: the projected customRules volume (when the policy has
	// any) and a spec-hash annotation so policy edits roll the Pod.
	var volumes []corev1.Volume
	var podAnnotations map[string]string
	if deps.dlp != nil {
		podAnnotations = map[string]string{
			"orch.bairavs.com/dlp-policy":      deps.dlp.name,
			"orch.bairavs.com/dlp-policy-hash": deps.dlp.hash,
		}
		if deps.dlp.customRulesCount > 0 && len(sidecars) > 0 {
			volumes = append(volumes, corev1.Volume{
				Name: dlpPolicyVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: DLPPolicyConfigMapName(deps.dlp.name),
						},
					},
				},
			})
		}
	}

	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      a.Name,
			Namespace: a.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					Containers: containers,
					Volumes:    volumes,
				},
			},
		},
	}
}

// dlpPolicyVolumeName is the Pod volume carrying the projected customRules.
const dlpPolicyVolumeName = "dlp-policy"

// -----------------------------------------------------------------------------
// DLP sidecar injection
// -----------------------------------------------------------------------------

// DLPSidecarImage is the container image used for the DLP proxy sidecar.
// Points at the same laptop-tag image the Compose target uses.
// Overriding lives in a future ConfigMap / operator flag; for now the
// value is a package const so unit tests can reference it.
const DLPSidecarImage = "bairav/dlp:laptop"

// dlpSidecarBasePort is the first localhost port the DLP sidecars listen
// on inside the Pod. Model[0] → 9100, Model[1] → 9101, and so on. The
// range 9100-9199 gives us headroom for 100 Models per Agent, which is
// far above anything practical. Deliberately not 9000 because grpc-web
// dev tools grab that port by default and the collision is a footgun
// in local kubectl port-forward sessions.
const dlpSidecarBasePort = int32(9100)

// computeModelEndpointsAndSidecars returns:
//   - modelEndpoints: name → URL the Agent container should USE for that
//     Model. When DLP is off, this is the Model's Service DNS (raw
//     upstream). When DLP is on, this is http://127.0.0.1:<port>/v1
//     pointing at the DLP sidecar which proxies to the real Service.
//   - sidecars: the DLP proxy Containers to add to the Pod spec. Empty
//     when DLP is off or when the Agent has no ModelRefs.
//
// This is the whole trick of the sidecar wiring: env vars still say
// BAIRAV_MODEL_X_ENDPOINT, the Agent container still reads them the
// same way, but the traffic now flows through the DLP proxy without
// any change to user code. The Model itself is unchanged too — the
// sidecar just forwards.
func computeModelEndpointsAndSidecars(a *orchv1alpha1.Agent, deps *resolvedWithFlag) (map[string]string, []corev1.Container) {
	endpoints := make(map[string]string, len(deps.models))
	if deps.dlp == nil {
		for _, m := range deps.models {
			endpoints[m.name] = m.endpoint
		}
		return endpoints, nil
	}

	sidecars := make([]corev1.Container, 0, len(deps.models))
	for i, m := range deps.models {
		port := dlpSidecarBasePort + int32(i)
		endpoints[m.name] = fmt.Sprintf("http://127.0.0.1:%d/v1", port)
		sidecars = append(sidecars, buildDLPSidecar(a, deps.dlp, m, port))
	}
	return endpoints, sidecars
}

// buildDLPSidecar constructs one DLP proxy Container that sits in front
// of exactly one Model endpoint. All configuration flows in via
// BAIRAV_DLP_* env vars — the sidecar image's ENTRYPOINT is `bairav dlp
// serve` and every flag has an env-var fallback (see cli/cmd/dlp.go).
//
// The sidecar's --upstream is the Model's Service DNS, so cross-Pod
// routing still uses standard K8s service discovery. Only the Agent's
// egress hop is intercepted.
func buildDLPSidecar(a *orchv1alpha1.Agent, dlpCfg *resolvedDLPPolicy, m resolvedModel, port int32) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "BAIRAV_DLP_UPSTREAM", Value: m.endpoint},
		{Name: "BAIRAV_DLP_LISTEN", Value: fmt.Sprintf(":%d", port)},
		{Name: "BAIRAV_DLP_ACTION", Value: string(dlpCfg.defaultAction)},
		{Name: "BAIRAV_DLP_AUDIT", Value: dlpCfg.auditPath},
		{Name: "BAIRAV_DLP_DIRECTION", Value: string(dlpCfg.direction)},
	}
	if len(dlpCfg.packs) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "BAIRAV_DLP_PACK",
			Value: strings.Join(dlpCfg.packs, ","),
		})
	} else {
		// The CLI defaults --pack to global-safe. A policy that lists no
		// packs means "custom rules only", so say so explicitly.
		env = append(env, corev1.EnvVar{Name: "BAIRAV_DLP_PACK", Value: "none"})
	}
	var mounts []corev1.VolumeMount
	if dlpCfg.customRulesCount > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "BAIRAV_DLP_POLICY",
			Value: DLPPolicyMountDir + "/" + DLPPolicyFileName,
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      dlpPolicyVolumeName,
			MountPath: DLPPolicyMountDir,
			ReadOnly:  true,
		})
	}
	// Container name dlp-<model> makes `kubectl logs <pod> -c dlp-<model>`
	// self-documenting and keeps the audit trail reviewable.
	return corev1.Container{
		Name:            dlpContainerName(m.name),
		Image:           DLPSidecarImage,
		Env:             env,
		VolumeMounts:    mounts,
		SecurityContext: hardenedSecurityContext(dlpUID, true),
		Ports: []corev1.ContainerPort{{
			Name:          fmt.Sprintf("dlp-%d", port),
			ContainerPort: port,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
}

// dlpContainerName derives a DNS-1123-safe container name from a Model
// name. K8s container names allow lowercase alnum + "-" but not "_" or
// "." — envKey (used for env-var names) is the opposite, so keep these
// helpers separate.
func dlpContainerName(modelName string) string {
	// Model names are already DNS-1123 (CRD validation); prefixing with
	// "dlp-" keeps within the 63-char container-name limit for any
	// reasonable Model name (limit becomes 59 usable chars post-prefix).
	name := "dlp-" + modelName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// buildService constructs the desired Service for the Agent's own
// HTTP surface.
func (r *AgentReconciler) buildService(a *orchv1alpha1.Agent) *corev1.Service {
	labels := map[string]string{
		"orch.bairavs.com/agent": a.Name,
	}
	svcType := a.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	port := a.Spec.Service.Port
	if port == 0 {
		port = a.Spec.Port
	}
	if port == 0 {
		port = 8000
	}
	containerPort := a.Spec.Port
	if containerPort == 0 {
		containerPort = 8000
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      a.Name,
			Namespace: a.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromInt32(containerPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *AgentReconciler) applyAgentDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get existing deploy: %w", err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *AgentReconciler) applyAgentService(ctx context.Context, desired *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get existing svc: %w", err)
	}
	existing.Labels = desired.Labels
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Type = desired.Spec.Type
	return r.Update(ctx, &existing)
}

// setPhaseWaiting patches the Agent's status to WaitingForDeps with a
// Condition explaining WHICH refs are unresolved. Called on every
// reconcile where deps aren't fully Ready so status stays accurate.
//
// If the Agent was previously Ready (i.e. a dep vanished after the
// Agent was up), we ALSO set a `Degraded=True` Condition to distinguish
// "was working, dep vanished" from "never came up in the first place."
// The phase stays WaitingForDeps in both cases — the difference is a
// separate Condition signal so `kubectl describe agent` shows the exact
// story. Pod is NOT scaled down: --cascade-agents on the undeploy CLI
// is the explicit path for that (see cli/cmd/model.go's undeploy).
func (r *AgentReconciler) setPhaseWaiting(ctx context.Context, a *orchv1alpha1.Agent, deps *resolvedWithFlag) error {
	base := a.DeepCopy()
	prevPhase := a.Status.Phase
	newStatus := a.Status
	newStatus.Phase = orchv1alpha1.AgentPhaseWaitingForDeps
	newStatus.ObservedGeneration = a.Generation
	newStatus.BoundModels = deps.modelNames()
	newStatus.BoundSkills = deps.skillNames()
	msg := waitingMessage(deps)
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:    "Progressing",
		Status:  metav1.ConditionTrue,
		Reason:  "AwaitingDependencies",
		Message: msg,
	})
	if prevPhase == orchv1alpha1.AgentPhaseReady {
		// Was Ready, dep vanished → surface as Degraded on top of the
		// WaitingForDeps phase change. Message names the removed refs
		// so the SOC/ops reviewer knows exactly what happened.
		meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
			Type:    "Degraded",
			Status:  metav1.ConditionTrue,
			Reason:  "DependencyRemoved",
			Message: msg,
		})
	} else {
		// Otherwise clear any stale Degraded condition (happens when
		// the Agent has been WaitingForDeps since first Create and
		// a previous reconcile had wrongly stamped Degraded).
		meta.RemoveStatusCondition(&newStatus.Conditions, "Degraded")
	}
	if !agentStatusChanged(a.Status, newStatus) {
		return nil
	}
	a.Status = newStatus
	return r.Status().Patch(ctx, a, client.MergeFrom(base))
}

// waitingMessage renders a human-readable summary of which deps are
// blocking. Kept concise (single line) since it lands in
// kubectl describe agent output.
func waitingMessage(deps *resolvedWithFlag) string {
	var parts []string
	if len(deps.missingModels) > 0 {
		parts = append(parts, fmt.Sprintf("missing Models: %s", strings.Join(deps.missingModels, ",")))
	}
	if len(deps.missingSkills) > 0 {
		parts = append(parts, fmt.Sprintf("missing Skills: %s", strings.Join(deps.missingSkills, ",")))
	}
	if len(deps.pendingModels) > 0 {
		parts = append(parts, fmt.Sprintf("pending Models: %s", strings.Join(deps.pendingModels, ",")))
	}
	if len(deps.pendingSkills) > 0 {
		parts = append(parts, fmt.Sprintf("pending Skills: %s", strings.Join(deps.pendingSkills, ",")))
	}
	if deps.dlpProblem != "" {
		parts = append(parts, deps.dlpProblem)
	}
	parts = append(parts, deps.refProblems...)
	if len(parts) == 0 {
		return "waiting"
	}
	return strings.Join(parts, "; ")
}

// computeStatus derives an AgentStatus once Deployment is being reconciled
// (i.e. all deps satisfied). Same shape as ModelReconciler/SkillReconciler.
func (r *AgentReconciler) computeStatus(a *orchv1alpha1.Agent, d *appsv1.Deployment, svc *corev1.Service, deps *resolvedWithFlag) orchv1alpha1.AgentStatus {
	out := a.Status.DeepCopy()
	if out == nil {
		out = &orchv1alpha1.AgentStatus{}
	}
	out.ObservedGeneration = a.Generation
	out.BoundModels = deps.modelNames()
	out.BoundSkills = deps.skillNames()

	port := a.Spec.Service.Port
	if port == 0 {
		port = a.Spec.Port
	}
	if port == 0 {
		port = 8000
	}
	out.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
		svc.Name, svc.Namespace, port)

	switch {
	case d.Status.ReadyReplicas >= 1:
		out.Phase = orchv1alpha1.AgentPhaseReady
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:   "Available",
			Status: metav1.ConditionTrue,
			Reason: "DeploymentReady",
			Message: fmt.Sprintf("%d/%d replicas ready; %d model deps + %d skill deps bound",
				d.Status.ReadyReplicas, d.Status.Replicas,
				len(deps.models), len(deps.skills)),
		})
		// If we were previously Degraded (dep came back after having
		// vanished), clear the Degraded condition — the Agent is now
		// serving with resolved refs again.
		meta.RemoveStatusCondition(&out.Conditions, "Degraded")
	case d.Status.UnavailableReplicas > 0 && d.Generation == d.Status.ObservedGeneration:
		out.Phase = orchv1alpha1.AgentPhaseDeploying
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "PodNotReady",
			Message: fmt.Sprintf("%d unavailable replica(s)", d.Status.UnavailableReplicas),
		})
	default:
		out.Phase = orchv1alpha1.AgentPhasePending
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "AwaitingScheduler",
			Message: "Deployment created; awaiting first pod",
		})
	}
	return *out
}

func agentStatusChanged(a, b orchv1alpha1.AgentStatus) bool {
	if a.Phase != b.Phase ||
		a.Endpoint != b.Endpoint ||
		a.ObservedGeneration != b.ObservedGeneration ||
		len(a.BoundModels) != len(b.BoundModels) ||
		len(a.BoundSkills) != len(b.BoundSkills) ||
		len(a.Conditions) != len(b.Conditions) {
		return true
	}
	for i := range a.BoundModels {
		if a.BoundModels[i] != b.BoundModels[i] {
			return true
		}
	}
	for i := range a.BoundSkills {
		if a.BoundSkills[i] != b.BoundSkills[i] {
			return true
		}
	}
	// Compare condition content too, not just count: when an Agent stays in
	// WaitingForDeps but the reason changes (e.g. "missing Model" → "no
	// AccessGrant"), the user must see the new message and fix command.
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

// envKey converts a ref name into an env-var-friendly UPPER_SNAKE token.
// "my-model" → "MY_MODEL"; "customer-api" → "CUSTOMER_API".
func envKey(name string) string {
	s := strings.ToUpper(name)
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
// Owns Deployment + Service (their status events trigger reconciles).
//
// ALSO Watches Model + Skill + DLPPolicy + AccessGrant events via mappers:
// when one changes, the mapper finds every Agent (in any namespace)
// that references it and enqueues a reconcile request. This is
// what makes the Agent notice its deps in real time — without it, a
// deleted Model would leave dependent Agents stuck at Ready until the
// controller's periodic resync fires (default ~10h, so effectively "never").
//
// The existing resolveDeps / setPhaseWaiting flow handles the state
// transition once triggered: Get(Model) returns NotFound → missingModels
// → allReady=false → phase flips to WaitingForDeps with a Condition
// naming the missing dep.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchv1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(
			&orchv1alpha1.Model{},
			handler.EnqueueRequestsFromMapFunc(r.agentsForModel),
		).
		Watches(
			&orchv1alpha1.Skill{},
			handler.EnqueueRequestsFromMapFunc(r.agentsForSkill),
		).
		Watches(
			&orchv1alpha1.DLPPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.agentsForDLPPolicy),
		).
		Watches(
			&orchv1alpha1.AccessGrant{},
			handler.EnqueueRequestsFromMapFunc(r.agentsForAccessGrant),
		).
		Complete(r)
}

// mappers match refs by EFFECTIVE namespace (ref.Namespace or the
// Agent's own), and list Agents cluster-wide, because an Agent in team-b
// may depend on a Model in team-a. A cluster-wide list from the informer
// cache is cheap; correctness beats a namespace-scoped shortcut here.

// refTargets reports whether any ref points at namespace/name.
func refTargets(a orchv1alpha1.Agent, refs []orchv1alpha1.LocalObjectReference, namespace, name string) bool {
	for _, ref := range refs {
		if ref.Name == name && refNamespace(&a, ref) == namespace {
			return true
		}
	}
	return false
}

// agentsForModel returns reconcile requests for every Agent (any
// namespace) that references the changed Model. Runs on CREATE / UPDATE /
// DELETE; on delete the mapper still gets the object's metadata, which is
// exactly when referencing Agents must flip to WaitingForDeps.
func (r *AgentReconciler) agentsForModel(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.agentsMatching(ctx, func(a orchv1alpha1.Agent) bool {
		return refTargets(a, a.Spec.ModelRefs, obj.GetNamespace(), obj.GetName())
	})
}

// agentsForSkill mirrors agentsForModel for Skill events.
func (r *AgentReconciler) agentsForSkill(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.agentsMatching(ctx, func(a orchv1alpha1.Agent) bool {
		return refTargets(a, a.Spec.SkillRefs, obj.GetNamespace(), obj.GetName())
	})
}

// agentsForDLPPolicy mirrors agentsForModel for DLPPolicy events, so a
// policy flipping Ready <-> Invalid re-gates every Agent that uses it.
func (r *AgentReconciler) agentsForDLPPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.agentsMatching(ctx, func(a orchv1alpha1.Agent) bool {
		return a.Spec.DLPRef != nil && a.Spec.DLPRef.Name == obj.GetName() &&
			refNamespace(&a, *a.Spec.DLPRef) == obj.GetNamespace()
	})
}

// agentsForAccessGrant wakes every Agent that references anything in the
// grant's namespace from another namespace. Deliberately broad (not just
// the grant's current From list): on revoke or edit, Agents that WERE
// covered must re-check too, and the old spec isn't available here.
func (r *AgentReconciler) agentsForAccessGrant(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetNamespace()
	return r.agentsMatching(ctx, func(a orchv1alpha1.Agent) bool {
		if a.Namespace == ns {
			return false
		}
		for _, ref := range append(append([]orchv1alpha1.LocalObjectReference{}, a.Spec.ModelRefs...), a.Spec.SkillRefs...) {
			if refNamespace(&a, ref) == ns {
				return true
			}
		}
		return false
	})
}

// agentsMatching lists Agents cluster-wide and returns reconcile requests
// for those matching the predicate. Best-effort: a failed list loses one
// wake-up; the next Agent event or requeue still converges.
func (r *AgentReconciler) agentsMatching(ctx context.Context, matches func(orchv1alpha1.Agent) bool) []reconcile.Request {
	var list orchv1alpha1.AgentList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, a := range list.Items {
		if matches(a) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace},
			})
		}
	}
	return reqs
}

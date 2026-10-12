// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package controller holds the reconcile loops for orch.bairavs.com kinds.
package controller

import (
	"context"
	"fmt"

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
	"sigs.k8s.io/controller-runtime/pkg/log"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// ModelReconciler reconciles a Model object into a Deployment + Service pair.
//
// The reconcile loop is intentionally idempotent and side-effect-free for
// unchanged inputs — controller-runtime calls Reconcile on every observed
// change, plus periodically, plus on every owned-resource event. We don't
// patch unless something is actually different.
type ModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orch.bairavs.com,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=models/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the controller's main loop.
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("model", req.NamespacedName)

	var m orchv1alpha1.Model
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner refs cascade-delete the Deployment + Service for us.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get model: %w", err)
	}

	// 0. Reject specs we can't serve
	// before creating anything, with a message that says how to fix it.
	if reason := validateModel(&m); reason != "" {
		base := m.DeepCopy()
		m.Status.Phase = orchv1alpha1.ModelPhaseFailed
		m.Status.ObservedGeneration = m.Generation
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: "Available", Status: metav1.ConditionFalse, Reason: "InvalidSpec", Message: reason,
		})
		if statusChanged(base.Status, m.Status) {
			if err := r.Status().Patch(ctx, &m, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
			}
		}
		logger.Info("invalid spec", "reason", reason)
		return ctrl.Result{}, nil
	}

	// 1. Reconcile the Deployment.
	desiredDeploy := r.buildDeployment(&m)
	if err := ctrl.SetControllerReference(&m, desiredDeploy, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set deploy owner: %w", err)
	}
	if err := r.applyDeployment(ctx, desiredDeploy); err != nil {
		return ctrl.Result{}, err
	}

	// 2. Reconcile the Service.
	desiredSvc := r.buildService(&m)
	if err := ctrl.SetControllerReference(&m, desiredSvc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set svc owner: %w", err)
	}
	if err := r.applyService(ctx, desiredSvc); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Refresh Status from observed Deployment.
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Name}, &deploy); err == nil {
		newStatus := r.computeStatus(&m, &deploy, desiredSvc)
		if statusChanged(m.Status, newStatus) {
			// Patch (merge) rather than Update. The reconciler Owns the
			// Deployment, so every Deployment status event fires another
			// reconcile; concurrent Status().Update calls each carry the
			// resourceVersion from their own Get and the second one loses
			// with "the object has been modified". A MergeFrom patch sets
			// no resourceVersion precondition, so it applies to the latest
			// version and the conflict goes away.
			base := m.DeepCopy()
			m.Status = newStatus
			if err := r.Status().Patch(ctx, &m, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
			}
		}
	}

	logger.V(1).Info("reconciled", "phase", m.Status.Phase)
	return ctrl.Result{}, nil
}

// buildDeployment constructs the desired Deployment for a Model.
func (r *ModelReconciler) buildDeployment(m *orchv1alpha1.Model) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "bairav-serving",
		"app.kubernetes.io/instance":   m.Name,
		"app.kubernetes.io/managed-by": "bairav-operator",
		"orch.bairavs.com/model":       m.Name,
	}
	port := m.Spec.Service.Port
	if port == 0 {
		port = 8080
	}
	if m.Spec.IsCloud() {
		return r.buildGatewayDeployment(m, labels, port)
	}
	image := m.Spec.Image
	if image == "" {
		image = "bairav/serving:laptop"
	}

	env := []corev1.EnvVar{
		{Name: "BAIRAV_REPO", Value: m.Spec.Repo},
		{Name: "BAIRAV_PORT", Value: fmt.Sprintf("%d", port)},
		{Name: "BAIRAV_HOST", Value: "0.0.0.0"},
	}
	if m.Spec.File != "" {
		env = append(env, corev1.EnvVar{Name: "BAIRAV_FILE", Value: m.Spec.File})
	}
	if m.Spec.ContextSize > 0 {
		env = append(env, corev1.EnvVar{Name: "BAIRAV_CTX_SIZE", Value: fmt.Sprintf("%d", m.Spec.ContextSize)})
	}
	if m.Spec.HFTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: m.Spec.HFTokenSecretRef.Name},
					Key:                  m.Spec.HFTokenSecretRef.Key,
				},
			},
		})
	}

	resources := m.Spec.Resources
	if resources.Requests == nil && resources.Limits == nil {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		}
	}

	one := int32(1)
	// Model pulls + loads can take many minutes; don't let K8s declare the
	// rollout failed before a big GGUF has had a fair chance.
	progressDeadline := int32(1800)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:                &one,
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			Strategy:                deploymentStrategy(m.Spec.UpgradeStrategy),
			ProgressDeadlineSeconds: &progressDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "serving",
						Image:           image,
						Env:             env,
						Resources:       resources,
						SecurityContext: servingSecurityContext(m.Spec.Image),
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: port,
							Protocol:      corev1.ProtocolTCP,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(port),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
							FailureThreshold:    60, // generous: model load can take >5min for big GGUFs
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(port),
								},
							},
							InitialDelaySeconds: 600, // wait until well past load before liveness kicks in
							PeriodSeconds:       30,
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "models",
							MountPath: "/models",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "models",
						// emptyDir for v1alpha1 — re-pulls on pod restart. PVC support
						// is the next iteration; keeps the MVP path zero-config.
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
}

// deploymentStrategy maps ModelSpec.UpgradeStrategy onto a Deployment
// strategy. Surge is written out explicitly rather than relying on
// the K8s defaults (25%/25%), so "new pod Ready before old pod goes" is a
// guarantee of ours, not an accident of rounding with replicas=1.
func deploymentStrategy(s string) appsv1.DeploymentStrategy {
	if s == orchv1alpha1.ModelUpgradeRecreate {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one},
	}
}

// rolloutComplete reports whether every replica runs the newest pod
// template and is available, with no old pods left. This is the moment an
// upgrade is done; ReadyReplicas >= 1 alone is also true mid-upgrade,
// because the OLD pod keeps serving.
func rolloutComplete(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want &&
		d.Status.AvailableReplicas == want &&
		d.Status.Replicas == want
}

// rolloutStalled reports K8s's own verdict that the rollout ran past
// ProgressDeadlineSeconds (e.g. the new model never became Ready).
func rolloutStalled(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

// buildService constructs the desired Service for a Model.
func (r *ModelReconciler) buildService(m *orchv1alpha1.Model) *corev1.Service {
	labels := map[string]string{
		"app.kubernetes.io/name":       "bairav-serving",
		"app.kubernetes.io/instance":   m.Name,
		"app.kubernetes.io/managed-by": "bairav-operator",
		"orch.bairavs.com/model":       m.Name,
	}
	port := m.Spec.Service.Port
	if port == 0 {
		port = 8080
	}
	svcType := m.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// applyDeployment creates or updates the Deployment in-cluster.
func (r *ModelReconciler) applyDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get existing deploy: %w", err)
	}
	// Patch spec; preserve the existing object's resourceVersion + status.
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// applyService creates or updates the Service in-cluster.
func (r *ModelReconciler) applyService(ctx context.Context, desired *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get existing svc: %w", err)
	}
	// Service spec is mostly immutable after creation; only sync labels + ports.
	existing.Labels = desired.Labels
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Type = desired.Spec.Type
	return r.Update(ctx, &existing)
}

// computeStatus derives a ModelStatus from observed Deployment state.
func (r *ModelReconciler) computeStatus(m *orchv1alpha1.Model, d *appsv1.Deployment, svc *corev1.Service) orchv1alpha1.ModelStatus {
	out := m.Status.DeepCopy()
	if out == nil {
		out = &orchv1alpha1.ModelStatus{}
	}
	out.ObservedGeneration = m.Generation

	port := m.Spec.Service.Port
	if port == 0 {
		port = 8080
	}
	out.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/v1",
		svc.Name, svc.Namespace, port)

	// Upgrade tracking. Served* only move once the rollout is
	// fully done, so "what is serving right now" is always truthful.
	switch {
	case rolloutComplete(d):
		out.ServedRepo, out.ServedFile = m.Spec.Repo, m.Spec.File
		meta.RemoveStatusCondition(&out.Conditions, "Upgrading")
	case rolloutStalled(d):
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type: "Upgrading", Status: metav1.ConditionFalse, Reason: "UpgradeStalled",
			Message: fmt.Sprintf("new spec (%s) did not become Ready before the progress deadline; still serving %s", m.Spec.Repo, out.ServedRepo),
		})
	case out.ServedRepo != "" && (out.ServedRepo != m.Spec.Repo || out.ServedFile != m.Spec.File || d.Status.UpdatedReplicas < d.Status.Replicas):
		msg := fmt.Sprintf("rolling out %s; %s keeps serving until it is Ready", m.Spec.Repo, out.ServedRepo)
		if m.Spec.UpgradeStrategy == orchv1alpha1.ModelUpgradeRecreate {
			msg = fmt.Sprintf("recreating with %s (%s stopped; short outage)", m.Spec.Repo, out.ServedRepo)
		}
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type: "Upgrading", Status: metav1.ConditionTrue, Reason: "RollingOut", Message: msg,
		})
	}

	switch {
	case d.Status.ReadyReplicas >= 1:
		out.Phase = orchv1alpha1.ModelPhaseReady
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "DeploymentReady",
			Message: fmt.Sprintf("%d/%d replicas ready", d.Status.ReadyReplicas, d.Status.Replicas),
		})
	case d.Status.UnavailableReplicas > 0 && d.Generation == d.Status.ObservedGeneration:
		out.Phase = orchv1alpha1.ModelPhaseLoading
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionFalse,
			Reason:  "Loading",
			Message: "Pod started; waiting for readiness probe (model loading or HF pull in progress)",
		})
	default:
		out.Phase = orchv1alpha1.ModelPhasePending
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionFalse,
			Reason:  "Pending",
			Message: "Reconciler is creating Deployment + Service",
		})
	}
	return *out
}

// statusChanged returns true if a is not deeply equal to b in ways the API
// would care about. Cheap shortcut to avoid no-op status patches.
func statusChanged(a, b orchv1alpha1.ModelStatus) bool {
	if a.Phase != b.Phase || a.Endpoint != b.Endpoint || a.ModelFile != b.ModelFile ||
		a.ServedRepo != b.ServedRepo || a.ServedFile != b.ServedFile {
		return true
	}
	if a.ObservedGeneration != b.ObservedGeneration {
		return true
	}
	if len(a.Conditions) != len(b.Conditions) {
		return true
	}
	// Content too: an upgrade can go RollingOut → UpgradeStalled without the
	// condition count changing, and the CLI waits on exactly that signal.
	for i := range a.Conditions {
		if a.Conditions[i].Type != b.Conditions[i].Type || a.Conditions[i].Status != b.Conditions[i].Status ||
			a.Conditions[i].Reason != b.Conditions[i].Reason || a.Conditions[i].Message != b.Conditions[i].Message {
			return true
		}
	}
	return false
}

// SetupWithManager registers this reconciler with the controller-runtime manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchv1alpha1.Model{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

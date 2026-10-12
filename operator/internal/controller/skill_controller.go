// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

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

// SkillReconciler reconciles a Skill object into a Deployment + Service pair.
//
// The shape is deliberately parallel to ModelReconciler — same reconcile
// loop, same status-patch (not update) semantics to avoid the resourceVersion
// race, same owner references so cascade-delete Just Works. Skill's
// reconcile is simpler because there's no HF pull inside the container
// and no llama-server warm-up; a Skill is either up or it isn't.
type SkillReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orch.bairavs.com,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=skills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orch.bairavs.com,resources=skills/finalizers,verbs=update

// Reconcile is the controller's main loop.
func (r *SkillReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("skill", req.NamespacedName)

	var s orchv1alpha1.Skill
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner refs cascade-delete the Deployment + Service for us.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get skill: %w", err)
	}

	// 1. Reconcile the Deployment.
	desiredDeploy := r.buildDeployment(&s)
	if err := ctrl.SetControllerReference(&s, desiredDeploy, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set deploy owner: %w", err)
	}
	if err := r.applyDeployment(ctx, desiredDeploy); err != nil {
		return ctrl.Result{}, err
	}

	// 2. Reconcile the Service.
	desiredSvc := r.buildService(&s)
	if err := ctrl.SetControllerReference(&s, desiredSvc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set svc owner: %w", err)
	}
	if err := r.applyService(ctx, desiredSvc); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Refresh Status from observed Deployment.
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &deploy); err == nil {
		newStatus := r.computeStatus(&s, &deploy, desiredSvc)
		if skillStatusChanged(s.Status, newStatus) {
			// Patch not Update: same reason as Model — the Owns
			// relationship fires cascading reconciles on Deployment
			// status changes, and two concurrent Status().Update() calls
			// each carry their own resourceVersion and race. A MergeFrom
			// patch sets no resourceVersion precondition, so whichever
			// lands second still applies cleanly.
			base := s.DeepCopy()
			s.Status = newStatus
			if err := r.Status().Patch(ctx, &s, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
			}
		}
	}

	logger.V(1).Info("reconciled", "phase", s.Status.Phase)
	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// build helpers
// -----------------------------------------------------------------------------

// buildDeployment constructs the desired Deployment for a Skill.
func (r *SkillReconciler) buildDeployment(s *orchv1alpha1.Skill) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "bairav-skill",
		"app.kubernetes.io/instance":   s.Name,
		"app.kubernetes.io/managed-by": "bairav-operator",
		"orch.bairavs.com/skill":       s.Name,
	}
	port := s.Spec.Port
	if port == 0 {
		port = 3000
	}
	healthPath := s.Spec.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}

	resources := s.Spec.Resources
	if resources.Requests == nil && resources.Limits == nil {
		// Skills tend to be lightweight proxies to tools/APIs, not
		// inference engines. Conservative defaults; users override via
		// spec.resources when they know they need more.
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

	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      "skill",
						Image:     s.Spec.Image,
						Env:       s.Spec.Env,
						Resources: resources,
						Ports: []corev1.ContainerPort{{
							Name:          "mcp",
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
							FailureThreshold:    12, // ~1min total — MCP servers start fast
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
					}},
				},
			},
		},
	}
}

// buildService constructs the desired Service for a Skill.
func (r *SkillReconciler) buildService(s *orchv1alpha1.Skill) *corev1.Service {
	labels := map[string]string{
		"orch.bairavs.com/skill": s.Name,
	}
	svcType := s.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	port := s.Spec.Service.Port
	if port == 0 {
		port = s.Spec.Port
	}
	if port == 0 {
		port = 3000
	}
	containerPort := s.Spec.Port
	if containerPort == 0 {
		containerPort = 3000
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "mcp",
				Port:       port,
				TargetPort: intstr.FromInt32(containerPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// applyDeployment creates or updates the Deployment in-cluster.
func (r *SkillReconciler) applyDeployment(ctx context.Context, desired *appsv1.Deployment) error {
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

// applyService creates or updates the Service in-cluster.
func (r *SkillReconciler) applyService(ctx context.Context, desired *corev1.Service) error {
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

// computeStatus derives a SkillStatus from observed Deployment state.
func (r *SkillReconciler) computeStatus(s *orchv1alpha1.Skill, d *appsv1.Deployment, svc *corev1.Service) orchv1alpha1.SkillStatus {
	out := s.Status.DeepCopy()
	if out == nil {
		out = &orchv1alpha1.SkillStatus{}
	}
	out.ObservedGeneration = s.Generation

	port := s.Spec.Service.Port
	if port == 0 {
		port = s.Spec.Port
	}
	if port == 0 {
		port = 3000
	}
	out.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
		svc.Name, svc.Namespace, port)

	switch {
	case d.Status.ReadyReplicas >= 1:
		out.Phase = orchv1alpha1.SkillPhaseReady
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "DeploymentReady",
			Message: fmt.Sprintf("%d/%d replicas ready", d.Status.ReadyReplicas, d.Status.Replicas),
		})
	case d.Status.UnavailableReplicas > 0 && d.Generation == d.Status.ObservedGeneration:
		out.Phase = orchv1alpha1.SkillPhaseDeploying
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "PodNotReady",
			Message: fmt.Sprintf("%d unavailable replica(s)", d.Status.UnavailableReplicas),
		})
	default:
		out.Phase = orchv1alpha1.SkillPhasePending
		meta.SetStatusCondition(&out.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "AwaitingScheduler",
			Message: "Deployment created; awaiting first pod",
		})
	}
	return *out
}

// skillStatusChanged is a shallow diff — the phase + endpoint changing is
// what matters. Conditions churn on their own timestamps; if only the
// LastTransitionTime moved we don't need to patch the whole object.
func skillStatusChanged(a, b orchv1alpha1.SkillStatus) bool {
	return a.Phase != b.Phase ||
		a.Endpoint != b.Endpoint ||
		a.ObservedGeneration != b.ObservedGeneration ||
		len(a.Conditions) != len(b.Conditions)
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
// Owns Deployment + Service so their status events trigger reconcile —
// same pattern as ModelReconciler.
func (r *SkillReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchv1alpha1.Skill{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

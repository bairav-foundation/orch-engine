// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package k8sclient

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// Scheme is the runtime scheme with both core K8s types and our v1alpha1 types
// registered. Exported so callers can register their own kinds if needed.
var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(orchv1alpha1.AddToScheme(Scheme))
}

// New returns a controller-runtime client pointed at the current kube context.
// Loads config from (in priority order):
//  1. In-cluster service-account credentials (rare for CLI but supported).
//  2. $KUBECONFIG env var.
//  3. $HOME/.kube/config.
//
// Returns a clear error if no config is reachable — typically "no kubectl
// context configured; run `kubectl config view` to check."
func New() (client.Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: Scheme})
}

// NewClientset returns a typed client-go clientset for the same context
// as New. Needed for the few calls controller-runtime's client doesn't
// cover — today only Pod log reads, which `bairav dashboard` uses to show
// the DLP sidecars' audit stream.
func NewClientset() (kubernetes.Interface, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// CurrentContext returns the active kubeconfig context name (e.g.
// "k3d-bairavs"), or "" if not resolvable. Used purely for the UX banner
// `bairav model deploy` prints before doing anything destructive.
func CurrentContext() string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := rules.Load()
	if err != nil {
		return ""
	}
	return cfg.CurrentContext
}

func loadConfig() (*rest.Config, error) {
	// 1. In-cluster.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	// 2 + 3. KUBECONFIG env or default location.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if env := os.Getenv("KUBECONFIG"); env != "" {
		rules.ExplicitPath = env
	} else if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(path); statErr == nil {
			rules.ExplicitPath = path
		}
	}

	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{})
	cfg, err := kubeconfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf(
			"no Kubernetes config reachable: %w (set $KUBECONFIG or run `kubectl config view` to debug)",
			err)
	}
	return cfg, nil
}

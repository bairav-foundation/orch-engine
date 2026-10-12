// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package k8sclient is a thin wrapper around controller-runtime's client
// that loads kubeconfig from the standard locations and registers the
// orch.bairavs.com/v1alpha1 types into its scheme.
//
// The CLI uses this from `bairav model deploy`, `bairav model undeploy`, and
// the K8s-side of `bairav ps`. It deliberately stays focused on the BAIRAVS
// CRDs — generic kubectl-like behaviour is not in scope (use kubectl).
//
// We import the operator's API package directly (cross-module within the
// go.work workspace) so the typed Model struct is the single source of
// truth — no schema drift between operator and CLI.
package k8sclient

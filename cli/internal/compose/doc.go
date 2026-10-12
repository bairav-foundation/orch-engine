// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package compose runs models as Docker Compose stacks for users without
// a Kubernetes cluster. It is the "laptop" peer to the K8s deploy path:
// same serving image (bairav/serving:laptop), same BAIRAV_* env contract,
// same UX (`bairav model deploy <repo> --target docker`), just a different
// orchestrator.
//
// State model. Each stack lives in ~/.bairav/compose/<name>/ and contains
// its rendered docker-compose.yaml. The authoritative liveness/listing
// source is Docker itself: every container we create carries
// `bairav.managed=true` and `bairav.name=<name>` labels, so `docker ps` can
// answer "what's running" without us maintaining a parallel state file
// that can drift.
//
// Why not the Docker Go SDK. Two reasons: it pulls in a large dependency
// graph (docker/docker, swarm types, etc.), and it requires the user have
// a daemon socket reachable from this process. The compose CLI is already
// present on every machine that has Docker Desktop, handles socket
// discovery for us, and gives identical UX to users running `docker
// compose` by hand.
package compose

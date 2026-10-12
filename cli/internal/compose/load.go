// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

// reading a Model stack's Spec back from the compose file we wrote,
// so `bairav model upgrade --target docker` can change only the model and
// keep everything else (port, DLP mode, policy mount, context size, image)
// exactly as the user deployed it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// composeFile is the subset of our generated docker-compose.yaml we read back.
type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
	} `yaml:"services"`
}

// LoadSpec reconstructs the Spec of an existing Model stack from its
// docker-compose.yaml. Only understands files Render produced.
func LoadSpec(name string) (Spec, error) {
	path, err := composeFilePath(name)
	if err != nil {
		return Spec{}, err
	}
	body, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Spec{}, fmt.Errorf("read %s: %w (was the model deployed with --target docker?)", path, err)
	}
	return parseSpec(name, body)
}

func parseSpec(name string, body []byte) (Spec, error) {
	var f composeFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		return Spec{}, fmt.Errorf("parse compose file: %w", err)
	}
	srv, ok := f.Services["serving"]
	if !ok {
		return Spec{}, fmt.Errorf("compose file has no serving service; not a bairav model stack")
	}
	s := Spec{
		Name:    name,
		Repo:    srv.Environment["BAIRAV_REPO"],
		File:    srv.Environment["BAIRAV_FILE"],
		HFToken: srv.Environment["HF_TOKEN"],
	}
	if p := srv.Environment["BAIRAV_DLP_PROVIDER"]; p != "" { // cloud stack
		s.Provider = p
		s.BaseURL = srv.Environment["BAIRAV_DLP_UPSTREAM"]
		s.DLPAction = srv.Environment["BAIRAV_DLP_ACTION"]
		if srv.Image != DefaultDLPImage {
			s.DLPImage = srv.Image
		}
		for _, port := range srv.Ports {
			n, ok, err := hostPortOf(port)
			if err != nil {
				return Spec{}, fmt.Errorf("port %q: %w", port, err)
			}
			if ok {
				s.HostPort = n
			}
		}
		if s.Repo == "" {
			return Spec{}, fmt.Errorf("compose file has no BAIRAV_REPO; not a bairav model stack")
		}
		return s, nil
	}
	if srv.Image != DefaultImage {
		s.Image = srv.Image
	}
	if v := srv.Environment["BAIRAV_CTX_SIZE"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Spec{}, fmt.Errorf("BAIRAV_CTX_SIZE %q: %w", v, err)
		}
		s.CtxSize = n
	}
	for _, v := range srv.Volumes {
		if host, ok := strings.CutSuffix(v, ":/models"); ok {
			s.ModelsHostDir = host
		}
	}
	ports := srv.Ports
	if dlp, ok := f.Services["dlp"]; ok {
		s.DLPMode = DLPModeEnforce
		if dlp.Image != DefaultDLPImage {
			s.DLPImage = dlp.Image
		}
		s.DLPAction = dlp.Environment["BAIRAV_DLP_ACTION"]
		for _, v := range dlp.Volumes {
			if host, ok := strings.CutSuffix(v, ":/etc/bairav/dlp-policy.yaml:ro"); ok {
				s.DLPPolicyHostPath = host
			}
		}
		ports = dlp.Ports // enforce mode publishes the host port on the proxy
	}
	for _, p := range ports {
		n, ok, err := hostPortOf(p)
		if err != nil {
			return Spec{}, fmt.Errorf("port %q: %w", p, err)
		}
		if ok {
			s.HostPort = n
		}
	}
	if s.Repo == "" {
		return Spec{}, fmt.Errorf("compose file has no BAIRAV_REPO; not a bairav model stack")
	}
	return s, nil
}

// PreviousModel is what `bairav model rollback --target docker` restores.
type PreviousModel struct {
	Repo string `json:"repo"`
	File string `json:"file,omitempty"`
	// Provider / BaseURL let a rollback cross providers.
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"baseURL,omitempty"`
	// At is when the upgrade away from this model happened.
	At time.Time `json:"at,omitempty"`
}

func previousPath(name string) (string, error) {
	dir, err := StackDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "previous-model.json"), nil
}

// SavePrevious records the model a docker stack ran before an upgrade.
func SavePrevious(name string, p PreviousModel) error {
	path, err := previousPath(name)
	if err != nil {
		return err
	}
	if p.At.IsZero() {
		p.At = time.Now().UTC()
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadPrevious returns the model recorded by the last upgrade.
func LoadPrevious(name string) (PreviousModel, error) {
	var p PreviousModel
	path, err := previousPath(name)
	if err != nil {
		return p, err
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return p, fmt.Errorf("no previous model recorded for %q (rollback needs a prior `bairav model upgrade`): %w", name, err)
	}
	err = json.Unmarshal(b, &p)
	return p, err
}

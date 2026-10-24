// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// cloud models. `bairav model deploy <model-id> --provider X` puts
// an OpenAI / Anthropic / OpenAI-compatible model behind the DLP gateway,
// on docker or k8s, with the same name/endpoint contract as local models.
// `bairav model upgrade --provider` swaps between local and cloud.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

const providerLocal = "local"

// addCloudFlags registers the provider flags on deploy and upgrade.
func addCloudFlags(c *cobra.Command) {
	c.Flags().String("provider", "", "where the model runs: local (default, Hugging Face) | openai | anthropic | openai-compatible")
	c.Flags().String("base-url", "", "provider API base URL (required for openai-compatible, e.g. https://host/v1)")
	c.Flags().String("api-key-secret", "", "k8s: existing Secret holding the API key, as name or name:key (default key: api-key)")
	c.Flags().String("api-key-env", "", "env var to read the API key from (default: OPENAI_API_KEY / ANTHROPIC_API_KEY, then BAIRAV_PROVIDER_API_KEY)")
}

// cloudOpts are the parsed provider flags.
type cloudOpts struct {
	provider   string // "local" when unset
	baseURL    string
	secretName string
	secretKey  string
	keyEnv     string
	changed    bool // any provider flag given on the command line
}

func (o cloudOpts) isCloud() bool { return o.provider != "" && o.provider != providerLocal }

// needsKey: openai-compatible endpoints (vLLM, Ollama, ...) are often keyless.
func (o cloudOpts) needsKey() bool { return o.isCloud() && o.provider != dlp.ProviderOpenAICompatible }

func readCloudOpts(cmd *cobra.Command) (cloudOpts, error) {
	var o cloudOpts
	get := func(name string) string {
		if cmd.Flags().Lookup(name) == nil {
			return ""
		}
		if cmd.Flags().Changed(name) {
			o.changed = true
		}
		v, _ := cmd.Flags().GetString(name)
		return strings.TrimSpace(v)
	}
	o.provider = strings.ToLower(get("provider"))
	o.baseURL = get("base-url")
	o.keyEnv = get("api-key-env")
	if ref := get("api-key-secret"); ref != "" {
		o.secretName, o.secretKey, _ = strings.Cut(ref, ":")
		if o.secretKey == "" {
			o.secretKey = "api-key"
		}
	}
	if o.provider == "" {
		o.provider = providerLocal
	}
	if o.provider != providerLocal && !dlp.ValidProvider(o.provider) {
		return o, fmt.Errorf("unknown --provider %q (use local, openai, anthropic or openai-compatible)", o.provider)
	}
	if !o.isCloud() && (o.baseURL != "" || o.secretName != "" || o.keyEnv != "") {
		return o, fmt.Errorf("--base-url / --api-key-* need --provider openai | anthropic | openai-compatible")
	}
	if o.provider == dlp.ProviderOpenAICompatible && o.baseURL == "" {
		return o, fmt.Errorf("--provider openai-compatible needs --base-url (e.g. http://my-vllm:8000/v1)")
	}
	return o, nil
}

// providerKey finds the API key in the environment. Returns the key and
// the variable it came from ("" when none is set).
func providerKey(o cloudOpts) (key, from string) {
	names := []string{o.keyEnv}
	if o.keyEnv == "" {
		switch o.provider {
		case dlp.ProviderOpenAI:
			names = []string{"OPENAI_API_KEY"}
		case dlp.ProviderAnthropic:
			names = []string{"ANTHROPIC_API_KEY"}
		default:
			names = nil
		}
		names = append(names, "BAIRAV_PROVIDER_API_KEY")
	}
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v, n
		}
	}
	return "", ""
}

func missingKeyErr(o cloudOpts, k8s bool) error {
	env := "BAIRAV_PROVIDER_API_KEY"
	switch o.provider {
	case dlp.ProviderOpenAI:
		env = "OPENAI_API_KEY"
	case dlp.ProviderAnthropic:
		env = "ANTHROPIC_API_KEY"
	}
	if k8s {
		return fmt.Errorf("--provider %s needs an API key: export %s=... (bairav stores it in a Secret only the gateway reads), or pass --api-key-secret <name[:key]>", o.provider, env)
	}
	return fmt.Errorf("--provider %s needs an API key: export %s=... (stored 0600 in the stack dir, never in the compose file)", o.provider, env)
}

// providerSecretName is per provider so a rollback across providers still
// finds its own key.
func providerSecretName(model, provider string) string { return model + "-" + provider + "-key" }

// applyCloudK8s fills spec.Provider / spec.Cloud, creating or updating the
// key Secret from the environment when no --api-key-secret was given.
func applyCloudK8s(ctx context.Context, c ctrlclient.Client, ns, model string, o cloudOpts, spec *orchv1alpha1.ModelSpec, out io.Writer) error {
	if !o.isCloud() {
		spec.Provider, spec.Cloud = providerLocal, nil
		return nil
	}
	cloud := &orchv1alpha1.ModelCloudSpec{BaseURL: o.baseURL}
	if spec.Cloud != nil && spec.Provider == o.provider { // keep DLP knobs set in YAML
		cloud.DLPAction, cloud.DLPPacks, cloud.Image = spec.Cloud.DLPAction, spec.Cloud.DLPPacks, spec.Cloud.Image
		if cloud.BaseURL == "" {
			cloud.BaseURL = spec.Cloud.BaseURL
		}
	}
	switch key, from := providerKey(o); {
	case o.secretName != "":
		cloud.APIKeySecretRef = &orchv1alpha1.SecretKeyRef{Name: o.secretName, Key: o.secretKey}
	case key != "":
		name := providerSecretName(model, o.provider)
		if err := upsertSecret(ctx, c, ns, name, "api-key", key); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Secret %s/%s holds the key from $%s (only the gateway pod reads it)\n", ns, name, from)
		cloud.APIKeySecretRef = &orchv1alpha1.SecretKeyRef{Name: name, Key: "api-key"}
	case spec.Cloud != nil && spec.Provider == o.provider && spec.Cloud.APIKeySecretRef != nil:
		cloud.APIKeySecretRef = spec.Cloud.APIKeySecretRef // same provider: reuse
	case o.needsKey():
		return missingKeyErr(o, true)
	}
	spec.Provider, spec.Cloud = o.provider, cloud
	return nil
}

func upsertSecret(ctx context.Context, c ctrlclient.Client, ns, name, key, value string) error {
	var s corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s)
	switch {
	case apierrors.IsNotFound(err):
		s = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "bairav"}},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{key: value},
		}
		if err := c.Create(ctx, &s); err != nil {
			return fmt.Errorf("create secret %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get secret %s: %w", name, err)
	}
	s.StringData = map[string]string{key: value}
	if err := c.Update(ctx, &s); err != nil {
		return fmt.Errorf("update secret %s: %w", name, err)
	}
	return nil
}

// deployCloudToCompose runs a cloud model as a single gateway container.
func deployCloudToCompose(ctx context.Context, cmd *cobra.Command, name, modelID string, port int, o cloudOpts, dlpAction string) error {
	out := cmd.OutOrStdout()
	key, from := providerKey(o)
	if key == "" && o.needsKey() && !providerKeyStored(name, o.provider) {
		return missingKeyErr(o, false)
	}
	spec := compose.Spec{Name: name, Repo: modelID, HostPort: port, Provider: o.provider,
		BaseURL: o.baseURL, DLPAction: dlpAction, APIKey: key}

	fmt.Fprintf(out, "Target:    docker (compose)\n")
	fmt.Fprintf(out, "Name:      %s\n", name)
	fmt.Fprintf(out, "Model:     %s:%s (cloud, via the bairav DLP gateway)\n", o.provider, modelID)
	if o.baseURL != "" {
		fmt.Fprintf(out, "Base URL:  %s\n", o.baseURL)
	}
	if from != "" {
		fmt.Fprintf(out, "API key:   from $%s, stored 0600 in the stack dir (not in the compose file)\n", from)
	}
	fmt.Fprintf(out, "DLP:       %s on every request and response; model pinned; inference paths only\n", orDefault(dlpAction, "redact"))
	fmt.Fprintf(out, "Endpoint:  %s\n\n", spec.Endpoint())
	if err := compose.Up(ctx, spec, out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Gateway up (no download: starts in seconds).\n")
	fmt.Fprintf(out, "  Talk to it:   bairav chat --endpoint %s\n", spec.Endpoint())
	fmt.Fprintf(out, "  Audit trail:  docker logs -f bairav-%s\n", name)
	fmt.Fprintf(out, "  Swap later:   bairav model upgrade %s --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --provider local\n", name)
	return nil
}

// providerKeyStored reports whether a docker stack already has a non-empty
// key file for provider (redeploys and upgrades can reuse it).
func providerKeyStored(stack, provider string) bool {
	dir, err := compose.StackDir(stack)
	if err != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, compose.ProviderEnvFile(provider)))
	return err == nil && fi.Size() > 0
}

// modelRef is what a Model serves: the unit `upgrade` swaps and
// `rollback` restores.
type modelRef struct {
	Repo     string                       `json:"repo"`
	File     string                       `json:"file,omitempty"`
	Provider string                       `json:"provider,omitempty"`
	Cloud    *orchv1alpha1.ModelCloudSpec `json:"cloud,omitempty"`
	// Revision and SHA256 pin a local model (commit + file hash). Records
	// written by v0.2.0 don't have them: that means "main", unpinned.
	Revision string `json:"revision,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

func refOf(s orchv1alpha1.ModelSpec) modelRef {
	return modelRef{Repo: s.Repo, File: s.File, Provider: orDefault(s.Provider, providerLocal), Cloud: s.Cloud,
		Revision: s.Revision, SHA256: s.SHA256}
}

func (r modelRef) isCloud() bool { return r.Provider != "" && r.Provider != providerLocal }

func (r modelRef) label() string {
	if r.isCloud() {
		return r.Provider + ":" + r.Repo
	}
	return modelLabel(r.Repo, r.File)
}

func (r modelRef) same(o modelRef) bool {
	base := func(c *orchv1alpha1.ModelCloudSpec) string {
		if c == nil {
			return ""
		}
		return c.BaseURL
	}
	// A pinned hash that differs is a different model (the repo moved on),
	// so an upgrade to it is real; an unpinned side doesn't count.
	sameBytes := r.SHA256 == "" || o.SHA256 == "" || r.SHA256 == o.SHA256
	return r.Repo == o.Repo && r.File == o.File && orDefault(r.Provider, providerLocal) == orDefault(o.Provider, providerLocal) &&
		base(r.Cloud) == base(o.Cloud) && sameBytes
}

func (r modelRef) encode() string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

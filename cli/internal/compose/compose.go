// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultImage is the serving image, the same one the operator uses on
// Kubernetes, so both targets run identical code. Dev builds use the local
// tag that `make image-serving` builds; release builds switch to the
// published image via UseReleaseImages. Override per stack via Spec.Image.
var DefaultImage = "bairav/serving:laptop"

// DefaultDLPImage hosts the `bairav dlp serve` entrypoint. Used when
// Spec.DLPMode == DLPModeEnforce and Spec.DLPImage is empty, and for cloud
// gateways.
var DefaultDLPImage = "bairav/dlp:laptop"

// ReleaseRegistry is where release images are published.
const ReleaseRegistry = "ghcr.io/bairav-foundation"

var releaseVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(alpha|beta|rc)(\.\d+)?)?$`)

// UseReleaseImages points the default images at the published
// ghcr.io/bairav-foundation/{serving,dlp}:<version> images when version is a
// release tag (v1.2.3, v1.2.3-rc.1). Dev builds (v0.0.0-dev+sha) keep the
// local tags. BAIRAV_SERVING_IMAGE / BAIRAV_DLP_IMAGE override either way.
func UseReleaseImages(version string) {
	if releaseVersionRe.MatchString(version) {
		DefaultImage = ReleaseRegistry + "/serving:" + version
		DefaultDLPImage = ReleaseRegistry + "/dlp:" + version
	}
	if v := os.Getenv("BAIRAV_SERVING_IMAGE"); v != "" {
		DefaultImage = v
	}
	if v := os.Getenv("BAIRAV_DLP_IMAGE"); v != "" {
		DefaultDLPImage = v
	}
}

// DefaultContainerPort is the port llama-server listens on inside the
// container (set by the image's ENV BAIRAV_PORT). The host-side port comes
// from Spec.HostPort so multiple stacks can run side by side.
const DefaultContainerPort = 8080

// DefaultDLPContainerPort is the port the DLP proxy listens on inside its
// container. Kept distinct from DefaultContainerPort so the two services
// can coexist on the same compose network without a collision.
const DefaultDLPContainerPort = 8090

// Label keys we apply to every container we create. These are what `bairav
// ps` and `model undeploy --target docker` use to find our stuff; do not
// rename without migrating existing user installs.
const (
	LabelManaged = "bairav.managed"
	LabelName    = "bairav.name"
	LabelRepo    = "bairav.repo"

	// LabelRole distinguishes containers we manage within one stack. Values:
	// "serving" (the model) and "dlp" (the DLP proxy in enforce mode). Used
	// so `bairav ps` can identify what each row is without introspecting
	// the image name.
	LabelRole = "bairav.role"

	RoleServing = "serving"
	RoleDLP     = "dlp"
)

// DLPMode controls whether/how the DLP proxy fronts the model in a compose
// stack. Encoded as a string enum (rather than a bool) so the semantics can
// grow without churning callers or on-disk formats.
//
//	off     — default. Single service (serving), published on HostPort. No
//	          DLP layer. Users can `bairav dlp serve` separately if they want.
//	on      — reserved. Same shape as off in v1; may add "hint about a
//	          running DLP proxy" behaviour in a later cut without a schema
//	          change.
//	enforce — two services in one compose stack: serving (unpublished, on
//	          the compose default network only) and dlp (published on
//	          HostPort). The model literally has no reachable host port —
//	          the DLP proxy is the only door.
type DLPMode string

// DLPMode values. See the DLPMode type doc for what each mode does.
const (
	DLPModeOff     DLPMode = "off"
	DLPModeOn      DLPMode = "on"
	DLPModeEnforce DLPMode = "enforce"
)

// Spec describes one model to run under docker compose. It mirrors the
// fields of the K8s Model CR's spec that matter at the serving layer, so a
// user moving a workload between targets only retargets the flag.
type Spec struct {
	// Name is the stack identifier. Used for the compose project name, the
	// container name, the state directory, and the LabelName label. Must be
	// DNS-1123 safe (the CLI sanitises HF repo slugs via k8sNameFromRepo).
	Name string

	// Repo is the HuggingFace repo slug, e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF".
	Repo string

	// File is an optional path inside the repo. Empty means the serving
	// image picks the smallest GGUF.
	File string

	// Revision is the commit (or branch) to pull; empty = main. SHA256 is
	// the file's expected hash: the serving container refuses a mismatch.
	Revision string
	SHA256   string

	// Image overrides the serving image. Empty = DefaultImage.
	Image string

	// HostPort is the host-side port the model's HTTP endpoint binds to.
	// 0 = let docker pick (rare for our UX; we prefer explicit ports).
	HostPort int

	// CtxSize sets BAIRAV_CTX_SIZE. 0 = backend default.
	CtxSize int

	// HFToken pass-through. Empty = unset. Public repos don't need it.
	HFToken string

	// ModelsHostDir is the host directory bind-mounted to /models inside
	// the container, so first-pull persists across down/up. Empty =
	// ~/.bairav/models (resolved in Up).
	ModelsHostDir string

	// DLPMode is the enforcement mode for the DLP proxy. Default (zero
	// value "" → treated as DLPModeOff) reproduces the original single-
	// service behaviour. Set to DLPModeEnforce to add a DLP proxy sidecar
	// in front of the model and make the model itself unreachable from
	// the host directly. See DLPMode docstring for values.
	DLPMode DLPMode

	// DLPImage overrides the DLP proxy container image. Empty =
	// DefaultDLPImage. Only consulted when DLPMode == DLPModeEnforce.
	DLPImage string

	// DLPAction is the default DLP action ("redact" | "block" |
	// "audit_only") passed to the proxy via BAIRAV_DLP_ACTION. Empty =
	// "redact". Per-rule action overrides in the policy file still win.
	DLPAction string

	// DLPPolicyHostPath is an OPTIONAL absolute path to a YAML policy file
	// on the host. When set + DLPMode==Enforce, the file is bind-mounted
	// read-only into the DLP container and passed via --policy so custom
	// rules load. Empty means "built-ins only."
	DLPPolicyHostPath string

	// Provider: "" / "local" = serve Repo from Hugging Face.
	// "openai" | "anthropic" | "openai-compatible" = Repo is the provider's
	// model id and the stack runs the DLP gateway instead (cloud.go).
	Provider string

	// BaseURL overrides the provider API URL (required for openai-compatible).
	BaseURL string

	// APIKey is written to provider-<provider>.env (0600), never into the
	// compose file. Empty keeps whatever key file the stack already has.
	APIKey string
}

// Endpoint returns the URL a host process should hit to talk to this stack.
func (s Spec) Endpoint() string {
	port := s.HostPort
	if port == 0 {
		port = DefaultContainerPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// image returns the resolved image string (Spec.Image or DefaultImage).
func (s Spec) image() string {
	if s.Image == "" {
		return DefaultImage
	}
	return s.Image
}

// -----------------------------------------------------------------------------
// State directory
// -----------------------------------------------------------------------------

// stateDir returns ~/.bairav/compose. $BAIRAV_COMPOSE_DIR overrides for tests.
func stateDir() (string, error) {
	if v := os.Getenv("BAIRAV_COMPOSE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".bairav", "compose"), nil
}

// StackDir returns the on-disk directory for a given stack name. Exported
// so the deploy command can print the path it just wrote.
//
// The name becomes a path component, a compose project name and a YAML
// value, so it is validated here, the one choke point every Up / Down /
// key-file / previous-model path goes through: no "../", no newlines.
func StackDir(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// stackNameRe is compose's own project-name rule ("bairav-" + name must pass it).
var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateName checks a stack name: lowercase letters, digits, "-" and "_",
// starting with a letter or digit, at most 63 characters.
func ValidateName(name string) error {
	if !stackNameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits, '-' or '_' (max 63, must start with a letter or digit)", name)
	}
	return nil
}

// validateRender rejects values that are written into the compose YAML
// unquoted (image refs, env keys): whitespace, quotes or control
// characters there could inject YAML keys (privileged: true, volumes ...).
func validateRender(image string, env map[string]string) error {
	if image != "" && strings.ContainsAny(image, " \t\r\n\"'#{}[],&*!|>%`") {
		return fmt.Errorf("invalid image reference %q", image)
	}
	for k := range env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
	}
	return nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// composeFilePath returns the path of the docker-compose.yaml for a stack.
func composeFilePath(name string) (string, error) {
	dir, err := StackDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docker-compose.yaml"), nil
}

// defaultModelsHostDir resolves ~/.bairav/models.
func defaultModelsHostDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bairav", "models"), nil
}

// -----------------------------------------------------------------------------
// Up / Down
// -----------------------------------------------------------------------------

// Up writes the stack's docker-compose.yaml and brings it up detached.
// Writes to stdout/stderr for user-visible output (docker's own log lines).
// Idempotent at the docker layer: re-running with the same spec is a no-op
// for compose; with a changed spec it reconciles to the new state.
func Up(ctx context.Context, spec Spec, stdout, stderr io.Writer) error {
	if spec.Name == "" {
		return fmt.Errorf("compose: spec.Name is required")
	}
	if spec.Repo == "" {
		return fmt.Errorf("compose: spec.Repo is required")
	}
	if err := validateRender(spec.Image, nil); err != nil {
		return err
	}
	if err := validateRender(spec.DLPImage, nil); err != nil {
		return err
	}
	if spec.IsCloud() {
		if err := writeProviderKey(spec); err != nil {
			return err
		}
		return upStack(ctx, spec.Name, Render(spec), stdout, stderr)
	}

	if spec.ModelsHostDir == "" {
		d, err := defaultModelsHostDir()
		if err != nil {
			return err
		}
		spec.ModelsHostDir = d
	}
	// The bind-mount target must exist before docker creates the container,
	// or Docker Desktop will silently auto-create it as root-owned, breaking
	// the non-root user inside the container.
	if err := os.MkdirAll(spec.ModelsHostDir, 0o755); err != nil {
		return fmt.Errorf("create models dir: %w", err)
	}

	return upStack(ctx, spec.Name, Render(spec), stdout, stderr)
}

// upStack is the shared write-file + `docker compose up -d` path used by
// Model, Skill and Agent stacks. stackName is the on-disk directory name
// and (prefixed with "bairav-") the compose project name.
func upStack(ctx context.Context, stackName, yaml string, stdout, stderr io.Writer) error {
	if err := EnsureNetwork(ctx); err != nil {
		return err
	}
	dir, err := StackDir(stackName)
	if err != nil {
		return err
	}
	// 0700/0600: the compose file can carry HF_TOKEN and env values.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create stack dir: %w", err)
	}
	composeFile := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(composeFile, []byte(yaml), 0o600); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}
	_ = os.Chmod(composeFile, 0o600) // tighten files written by older versions
	args := []string{"compose", "-f", composeFile, "-p", "bairav-" + stackName, "up", "-d", "--remove-orphans"}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// EnsureNetwork creates the shared `bairav` bridge network if it doesn't
// exist. Every bairav stack declares it `external: true`, so compose
// refuses to start without it. Idempotent.
func EnsureNetwork(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", SharedNetwork).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "network", "create",
		"--label", LabelManaged+"=true", SharedNetwork).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("docker network create %s: %v: %s", SharedNetwork, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Down stops and removes the stack. Tries the rendered compose file first
// (preferred path); falls back to `docker compose -p` so we can clean up
// even if the user has deleted the stack directory by hand.
func Down(ctx context.Context, name string, stdout, stderr io.Writer) error {
	composeFile, err := composeFilePath(name)
	if err != nil {
		return err
	}
	var args []string
	if _, statErr := os.Stat(composeFile); statErr == nil {
		args = []string{"compose", "-f", composeFile, "-p", "bairav-" + name, "down", "-v"}
	} else {
		args = []string{"compose", "-p", "bairav-" + name, "down", "-v"}
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose down: %w", err)
	}
	// Remove the stack directory after a successful tear-down.
	if dir, derr := StackDir(name); derr == nil {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// -----------------------------------------------------------------------------
// List (for `bairav ps`)
// -----------------------------------------------------------------------------

// Stack is one running (or stopped) bairav-managed container, as observed
// from `docker ps`.
type Stack struct {
	Name     string // from LabelName
	Repo     string // from LabelRepo
	Image    string
	Status   string    // raw docker status string, e.g. "Up 3 minutes (healthy)"
	Created  time.Time // container creation time; zero if docker's format changes
	HostPort int       // host-side port we extracted from the Ports column
	Endpoint string    // http://127.0.0.1:<HostPort>/v1, computed

	// Graph fields. Kind is model | skill | agent (containers created
	// by older versions carry no kind label and are reported as model). Role is
	// serving | dlp | skill | agent. NetEndpoint is the URL other
	// containers use on the shared `bairav` network; empty on containers
	// that aren't a stack's door (or older stacks, which never joined
	// the shared network). Labels is the raw label map for ref lookups.
	Kind        string
	Role        string
	NetEndpoint string
	Labels      map[string]string
}

// dockerPSEntry is the shape of one record from `docker ps --format
// '{{json .}}'`. We only declare the fields we read; docker adds more.
type dockerPSEntry struct {
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	Labels string `json:"Labels"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
	// CreatedAt looks like "2026-10-03 13:00:00 -0400 EDT".
	CreatedAt string `json:"CreatedAt"`
}

// parseDockerTime parses docker ps's CreatedAt. Zero time on failure: age
// is a nicety and must never break listing.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// List returns every bairav-managed container Docker knows about. Returns
// nil, nil when Docker isn't installed or isn't running — this is a "best
// effort" listing for `bairav ps`, not a critical path. The caller decides
// whether to surface the absence to the user.
func List(ctx context.Context) ([]Stack, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label="+LabelManaged+"=true",
		"--format", "{{json .}}")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Daemon down, no socket, etc. — treat as "nothing to show".
		return nil, nil
	}

	var out []Stack
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var entry dockerPSEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		labels := parseLabels(entry.Labels)
		if labels[LabelManaged] != "true" {
			continue
		}
		s := Stack{
			Name:        labels[LabelName],
			Repo:        labels[LabelRepo],
			Image:       entry.Image,
			Status:      entry.Status,
			Created:     parseDockerTime(entry.CreatedAt),
			Kind:        labels[LabelKind],
			Role:        labels[LabelRole],
			NetEndpoint: labels[LabelEndpoint],
			Labels:      labels,
		}
		if s.Kind == "" {
			s.Kind = KindModel
		}
		if s.Name == "" {
			// Containers we created always carry LabelName; skip anything
			// that doesn't to avoid showing partially-corrupted entries.
			continue
		}
		s.HostPort = extractHostPort(entry.Ports)
		if s.HostPort > 0 {
			if s.Kind == KindModel {
				s.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", s.HostPort)
			} else {
				// Skills and Agents don't speak the OpenAI /v1 surface.
				s.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", s.HostPort)
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return kindOrder(out[i].Kind) < kindOrder(out[j].Kind)
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

func kindOrder(k string) int {
	switch k {
	case KindModel:
		return 0
	case KindSkill:
		return 1
	case KindAgent:
		return 2
	}
	return 3
}

// parseLabels turns docker's "k=v,k=v" label string into a map. Docker
// uses commas as separators and does not escape commas inside values; our
// own labels (set by Render) contain no commas, which we rely on here.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		out[pair[:eq]] = pair[eq+1:]
	}
	return out
}

// extractHostPort pulls the first host-side port out of docker's Ports
// column, which looks like "0.0.0.0:8088->8080/tcp, [::]:8088->8080/tcp".
// Returns 0 if we can't parse it (container stopped, no port published).
func extractHostPort(ports string) int {
	for _, mapping := range strings.Split(ports, ",") {
		mapping = strings.TrimSpace(mapping)
		// Look for "->" — left side has the host bind, right side the container port.
		arrow := strings.Index(mapping, "->")
		if arrow < 0 {
			continue
		}
		hostSide := mapping[:arrow]
		// hostSide is "0.0.0.0:8088" or "[::]:8088".
		colon := strings.LastIndexByte(hostSide, ':')
		if colon < 0 {
			continue
		}
		portStr := hostSide[colon+1:]
		port := 0
		for _, ch := range portStr {
			if ch < '0' || ch > '9' {
				port = 0
				break
			}
			port = port*10 + int(ch-'0')
		}
		if port > 0 {
			return port
		}
	}
	return 0
}

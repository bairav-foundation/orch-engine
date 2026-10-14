// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeDoc is the subset of the compose schema the tests assert on.
type composeDoc struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		Labels      map[string]string `yaml:"labels"`
		DependsOn   []string          `yaml:"depends_on"`
		Volumes     []string          `yaml:"volumes"`
		Networks    map[string]struct {
			Aliases []string `yaml:"aliases"`
		} `yaml:"networks"`
	} `yaml:"services"`
	Networks map[string]struct {
		External bool   `yaml:"external"`
		Name     string `yaml:"name"`
	} `yaml:"networks"`
}

// parse fails the test if the rendered file isn't valid YAML — the check
// every hand-built template needs and the older tests never had.
func parse(t *testing.T, s string) composeDoc {
	t.Helper()
	var d composeDoc
	if err := yaml.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", err, s)
	}
	return d
}

func TestRenderModel_OffMode_JoinsSharedNetwork(t *testing.T) {
	d := parse(t, Render(Spec{Name: "qwen", Repo: "Qwen/x", HostPort: 8088, ModelsHostDir: "/m"}))
	svc := d.Services["serving"]
	if got := svc.Networks[SharedNetwork].Aliases; len(got) != 1 || got[0] != "model-qwen" {
		t.Errorf("serving aliases = %v, want [model-qwen]", got)
	}
	if svc.Labels[LabelEndpoint] != "http://model-qwen:8080/v1" {
		t.Errorf("endpoint label = %q", svc.Labels[LabelEndpoint])
	}
	if svc.Labels[LabelKind] != KindModel {
		t.Errorf("kind label = %q", svc.Labels[LabelKind])
	}
	if n := d.Networks[SharedNetwork]; !n.External || n.Name != SharedNetwork {
		t.Errorf("shared network must be declared external, got %+v", n)
	}
}

// TestRenderModel_EnforceMode_ServingNotOnSharedNetwork is the security
// property: in enforce mode only the DLP proxy joins the shared network.
// If serving joined it, any Agent could bypass DLP by dialling the model.
func TestRenderModel_EnforceMode_ServingNotOnSharedNetwork(t *testing.T) {
	d := parse(t, Render(Spec{Name: "qwen", Repo: "Qwen/x", HostPort: 8088, ModelsHostDir: "/m", DLPMode: DLPModeEnforce}))
	if _, on := d.Services["serving"].Networks[SharedNetwork]; on {
		t.Fatal("serving must NOT join the shared network in enforce mode (DLP bypass)")
	}
	if d.Services["serving"].Labels[LabelEndpoint] != "" {
		t.Error("serving must not advertise an endpoint in enforce mode")
	}
	dlp := d.Services["dlp"]
	if got := dlp.Networks[SharedNetwork].Aliases; len(got) != 1 || got[0] != "model-qwen" {
		t.Errorf("dlp aliases = %v, want [model-qwen]", got)
	}
	if _, on := dlp.Networks["default"]; !on {
		t.Error("dlp must stay on the stack default network to reach serving")
	}
	if dlp.Labels[LabelEndpoint] != "http://model-qwen:8090/v1" {
		t.Errorf("dlp endpoint label = %q, want the proxy port 8090", dlp.Labels[LabelEndpoint])
	}
}

func TestRenderSkill(t *testing.T) {
	d := parse(t, RenderSkill(SkillSpec{Name: "echo", Image: "bairav/mcp-echo:v1", Port: 3000}))
	svc := d.Services["skill"]
	if svc.Image != "bairav/mcp-echo:v1" {
		t.Errorf("image = %q", svc.Image)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != "127.0.0.1::3000" {
		t.Errorf("hostPort 0 should publish on a docker-picked port; ports=%v", svc.Ports)
	}
	if svc.Labels[LabelEndpoint] != "http://skill-echo:3000" || svc.Labels[LabelTransport] != "sse" {
		t.Errorf("labels = %v", svc.Labels)
	}
	if got := svc.Networks[SharedNetwork].Aliases; len(got) != 1 || got[0] != "skill-echo" {
		t.Errorf("aliases = %v", got)
	}
}

func TestRenderAgent_NoDLP(t *testing.T) {
	d := parse(t, RenderAgent(AgentSpec{
		Name: "support", Image: "img:v1", Port: 8000,
		Env:    map[string]string{"ANTHROPIC_MODEL": "x"},
		Models: []ResolvedModel{{Name: "qwen2-5-0-5b", Endpoint: "http://model-qwen2-5-0-5b:8080/v1"}},
		Skills: []ResolvedSkill{{Name: "echo", Endpoint: "http://skill-echo:3000", Transport: "sse"}},
	}))
	if len(d.Services) != 1 {
		t.Fatalf("no DLP → 1 service, got %d", len(d.Services))
	}
	env := d.Services["agent"].Environment
	want := map[string]string{
		"BAIRAV_MODEL_QWEN2_5_0_5B_ENDPOINT": "http://model-qwen2-5-0-5b:8080/v1",
		"BAIRAV_SKILL_ECHO_ENDPOINT":         "http://skill-echo:3000",
		"BAIRAV_SKILL_ECHO_TRANSPORT":        "sse",
		"ANTHROPIC_MODEL":                    "x",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	labels := d.Services["agent"].Labels
	if labels[LabelRefModelPrefix+"qwen2-5-0-5b"] != "true" || labels[LabelRefSkillPrefix+"echo"] != "true" {
		t.Errorf("ref labels missing: %v", labels)
	}
}

func TestRenderAgent_WithDLP(t *testing.T) {
	d := parse(t, RenderAgent(AgentSpec{
		Name: "support", Image: "img:v1",
		Models: []ResolvedModel{{Name: "qwen", Endpoint: "http://model-qwen:8080/v1"}},
		DLP:    &AgentDLP{Packs: []string{"global-safe", "india"}, PolicyHostPath: "/abs/p.yaml"},
	}))
	agent := d.Services["agent"]
	if agent.Environment["BAIRAV_MODEL_QWEN_ENDPOINT"] != "http://dlp-qwen:8090/v1" {
		t.Errorf("agent must dial the in-stack DLP proxy, got %q", agent.Environment["BAIRAV_MODEL_QWEN_ENDPOINT"])
	}
	if len(agent.DependsOn) != 1 || agent.DependsOn[0] != "dlp-qwen" {
		t.Errorf("depends_on = %v", agent.DependsOn)
	}
	dlp, ok := d.Services["dlp-qwen"]
	if !ok {
		t.Fatal("missing dlp-qwen service")
	}
	for k, v := range map[string]string{
		"BAIRAV_DLP_UPSTREAM":  "http://model-qwen:8080/v1",
		"BAIRAV_DLP_PACK":      "global-safe,india",
		"BAIRAV_DLP_ACTION":    "redact",
		"BAIRAV_DLP_DIRECTION": "both",
		"BAIRAV_DLP_POLICY":    "/etc/bairav/dlp-policy.yaml",
	} {
		if dlp.Environment[k] != v {
			t.Errorf("dlp env %s = %q, want %q", k, dlp.Environment[k], v)
		}
	}
	if len(dlp.Volumes) != 1 || !strings.HasSuffix(dlp.Volumes[0], ":ro") {
		t.Errorf("policy must mount read-only, got %v", dlp.Volumes)
	}
	if len(dlp.Ports) != 0 {
		t.Errorf("DLP proxy must not publish a host port, got %v", dlp.Ports)
	}
	if dlp.Labels[LabelRole] != RoleDLP || dlp.Labels[LabelEndpoint] != "" {
		t.Errorf("dlp labels = %v (must be role=dlp, no endpoint)", dlp.Labels)
	}
}

func TestRenderAgent_DLPNoPacksMeansNone(t *testing.T) {
	d := parse(t, RenderAgent(AgentSpec{
		Name: "a", Image: "i",
		Models: []ResolvedModel{{Name: "m", Endpoint: "http://model-m:8080/v1"}},
		DLP:    &AgentDLP{},
	}))
	if got := d.Services["dlp-m"].Environment["BAIRAV_DLP_PACK"]; got != "none" {
		t.Errorf("BAIRAV_DLP_PACK = %q, want none", got)
	}
}

func TestDependentAgentsAndFindDoor(t *testing.T) {
	stacks := []Stack{
		{Name: "qwen", Kind: KindModel, Role: RoleServing, NetEndpoint: "http://model-qwen:8080/v1"},
		{Name: "old", Kind: KindModel, Role: RoleServing}, // older: no endpoint
		{Name: "a1", Kind: KindAgent, Role: RoleAgent, Labels: map[string]string{LabelRefModelPrefix + "qwen": "true"}},
		{Name: "a1", Kind: KindAgent, Role: RoleDLP, Labels: map[string]string{}},
		{Name: "a2", Kind: KindAgent, Role: RoleAgent, Labels: map[string]string{LabelRefSkillPrefix + "echo": "true"}},
	}
	if got := DependentAgents(stacks, LabelRefModelPrefix, "qwen"); len(got) != 1 || got[0] != "a1" {
		t.Errorf("model dependents = %v, want [a1]", got)
	}
	if got := DependentAgents(stacks, LabelRefSkillPrefix, "echo"); len(got) != 1 || got[0] != "a2" {
		t.Errorf("skill dependents = %v, want [a2]", got)
	}
	if FindDoor(stacks, KindModel, "qwen") == nil {
		t.Error("qwen door not found")
	}
	if FindDoor(stacks, KindModel, "old") != nil || FindAny(stacks, KindModel, "old") == nil {
		t.Error("older model: FindDoor must miss, FindAny must hit")
	}
}

func TestEnvKey(t *testing.T) {
	if got := EnvKey("qwen2-5-0-5b"); got != "QWEN2_5_0_5B" {
		t.Errorf("EnvKey = %q", got)
	}
}

func TestRenderSkillWithDLP(t *testing.T) {
	d := parse(t, RenderSkill(SkillSpec{Name: "echo", Image: "bairav/mcp-echo:v1", Port: 3000,
		DLP: &SkillDLP{Packs: []string{"global-safe", "india"}}}))
	if len(d.Services) != 2 {
		t.Fatalf("want skill + dlp, got %d services", len(d.Services))
	}
	skill, dlp := d.Services["skill"], d.Services["dlp"]
	if len(skill.Ports) != 0 || skill.Labels[LabelEndpoint] != "" {
		t.Errorf("the server must not be published or be the door: ports=%v labels=%v", skill.Ports, skill.Labels)
	}
	if _, onShared := skill.Networks[SharedNetwork]; onShared {
		t.Error("the server must stay on the stack network only")
	}
	e := dlp.Environment
	if e["BAIRAV_DLP_MODE"] != "mcp" || e["BAIRAV_DLP_UPSTREAM"] != "http://skill:3000" || e["BAIRAV_DLP_PACK"] != "global-safe,india" {
		t.Errorf("dlp env = %v", e)
	}
	if dlp.Labels[LabelEndpoint] != "http://skill-echo:3000" || dlp.Labels[LabelRole] != RoleDLP {
		t.Errorf("dlp should be the door: %v", dlp.Labels)
	}
	if got := dlp.Networks[SharedNetwork].Aliases; len(got) != 1 || got[0] != "skill-echo" {
		t.Errorf("aliases = %v", got)
	}
}

func TestRenderRemoteSkill(t *testing.T) {
	out := RenderSkill(SkillSpec{Name: "gh", URL: "https://mcp.example.com/mcp", Transport: "http"})
	d := parse(t, out)
	gw, ok := d.Services["gateway"]
	if !ok || len(d.Services) != 1 {
		t.Fatalf("want one gateway service: %v", d.Services)
	}
	e := gw.Environment
	if e["BAIRAV_DLP_UPSTREAM"] != "https://mcp.example.com" || e["BAIRAV_DLP_ACTION"] != "audit" {
		t.Errorf("gateway env = %v", e)
	}
	if e["BAIRAV_MCP_UPSTREAM_TOKEN"] != "${"+MCPTokenEnv+":-}" {
		t.Errorf("token must be interpolated from the shell, not written: %q", e["BAIRAV_MCP_UPSTREAM_TOKEN"])
	}
	if gw.Labels[LabelEndpoint] != "http://skill-gh:3000/mcp" {
		t.Errorf("endpoint keeps the remote path: %q", gw.Labels[LabelEndpoint])
	}
}

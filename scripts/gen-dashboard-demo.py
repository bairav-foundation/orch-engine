# Regenerates cli/internal/dashboard/assets/demo.json (the scripted dashboard demo).
# Usage: python3 scripts/gen-dashboard-demo.py
import json, copy
V="v0.2.0"; CTX="demo-cluster"
def model(ns,name,repo,phase="Ready"):
    return {"target":"k8s","namespace":ns,"name":name,"source":repo,"phase":phase,
            "endpoint":f"http://{name}.{ns}.svc.cluster.local:8080/v1" if phase=="Ready" else ""}
def skill(ns,name,img,phase="Ready"):
    return {"target":"k8s","namespace":ns,"name":name,"source":img,"phase":phase,"transport":"sse",
            "endpoint":f"http://{name}.{ns}.svc.cluster.local:3000" if phase=="Ready" else ""}
def agent(ns,name,img,models,skills,phase,bm,bs,dlp="",msg=""):
    return {"target":"k8s","namespace":ns,"name":name,"image":img,"phase":phase,"models":models,"skills":skills,
            "boundModels":bm,"boundSkills":bs,"dlpPolicy":dlp,"message":msg,
            "endpoint":f"http://{name}.{ns}.svc.cluster.local:8000" if phase=="Ready" else ""}
def snap(models=(),skills=(),agents=(),pols=(),grants=(),audit=()):
    return {"version":V,"context":CTX,"generatedAt":"","mode":"demo","models":list(models),"skills":list(skills),
            "agents":list(agents),"dlpPolicies":list(pols),"grants":list(grants),"audit":list(audit)}
QWEN=model("shared-models","qwen","Qwen/Qwen2.5-0.5B-Instruct-GGUF")
QWEN_LOAD=model("shared-models","qwen","Qwen/Qwen2.5-0.5B-Instruct-GGUF","Pulling")
LLAMA=model("team-b","llama-3b","meta-llama/Llama-3.2-3B-Instruct-GGUF")
ECHO=skill("team-a","tickets","bairav/mcp-echo:v1")
SEARCH=skill("team-a","kb-search","ghcr.io/team-a/mcp-search:v2")
HINT="no AccessGrant in shared-models lets team-a use Model qwen (owner runs: bairav grant team-a --model qwen -n shared-models)"
SUP_WAIT=agent("team-a","support-bot","ghcr.io/team-a/support-bot:v1",["shared-models/qwen"],["tickets","kb-search"],"WaitingForDeps",[],["kb-search","tickets"],"",HINT)
SUP=agent("team-a","support-bot","ghcr.io/team-a/support-bot:v1",["shared-models/qwen"],["tickets","kb-search"],"Ready",["shared-models/qwen"],["kb-search","tickets"])
SUP_DLP=dict(SUP,dlpPolicy="prod-guardrails")
ANA=agent("team-b","analytics-bot","ghcr.io/team-b/analytics:v3",["llama-3b","shared-models/qwen"],[],"Ready",["llama-3b","shared-models/qwen"],[],"prod-guardrails")
G_A={"namespace":"shared-models","name":"allow-team-a","from":["team-a"],"to":["Model/qwen"]}
G_B={"namespace":"shared-models","name":"allow-team-b","from":["team-b"],"to":["Model/qwen"]}
POL_A={"namespace":"team-a","name":"prod-guardrails","phase":"Ready","direction":"both","action":"redact","packs":["global-safe","india"],"rules":7}
POL_B=dict(POL_A,namespace="team-b",packs=["global-safe"],rules=4)
POL_S=dict(POL_A,namespace="shared-models",name="model-guard")
QWEN_DLP=dict(QWEN,dlpPolicy="model-guard")
# DLP on tool calls, and a remote MCP server behind a gateway.
ECHO_DLP=dict(ECHO,dlpPolicy="prod-guardrails")
SEARCH_DLP=dict(SEARCH,dlpPolicy="prod-guardrails")
REMOTE=dict(skill("team-a","crm","https://mcp.crm.example.com/mcp"),transport="http",dlpPolicy="prod-guardrails",
            endpoint="http://crm.team-a.svc.cluster.local:3000/mcp")
SKILLS_DLP=[ECHO_DLP,SEARCH_DLP,REMOTE]
SUP_CRM=dict(SUP_DLP,skills=["tickets","kb-search","crm"],boundSkills=["crm","kb-search","tickets"])
# Upgrade with an eval gate: the bigger model loads next to the old one,
# fails the team's suite, and is rolled back.
QWEN_TRY=dict(QWEN_DLP,source="Qwen/Qwen3-1.7B-GGUF",phase="Loading",endpoint="")
def ev(t,ns,a,m,act,labels,resp=(),lat=140):
    return {"time":f"2026-10-02T10:{t}Z","namespace":ns,"agent":a,"model":m,"direction":"both","action":act,
            "labels":list(labels),"responseLabels":list(resp),"latencyMs":lat}
A1=[ev("14:03","team-a","support-bot","qwen","redact",["AADHAAR"]),
    ev("14:09","team-a","support-bot","qwen","redact",[],["EMAIL"],212),
    ev("14:12","team-a","support-bot","qwen","audit_only",[],[],98)]
A2=A1+[ev("14:20","team-b","analytics-bot","llama-3b","block",["AWS_ACCESS_KEY"],(),11),
       ev("14:24","team-b","analytics-bot","qwen","redact",["CREDIT_CARD"],(),175)]
frames=[
 ("A fresh cluster with the BAIRAVS operator. Nothing deployed yet.",3500,snap()),
 ("bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --namespace shared-models: the model is pulling.",3500,snap([QWEN_LOAD])),
 ("The model is Ready, served with an OpenAI-compatible API, shared by the platform team.",4000,snap([QWEN])),
 ("team-a deploys two MCP Skills: a ticket tool and a knowledge-base search.",4000,snap([QWEN],[ECHO,SEARCH])),
 ("team-a's agent asks for the shared model. No grant yet, so it waits and says exactly why (click it).",6000,snap([QWEN],[ECHO,SEARCH],[SUP_WAIT])),
 ("The model owner runs bairav grant team-a --model qwen. The agent rolls in seconds.",5000,snap([QWEN],[ECHO,SEARCH],[SUP],[],[G_A])),
 ("Attach a DLPPolicy. Prompts and answers are scanned for identifiers: Aadhaar redacted going in, email redacted coming back.",6500,snap([QWEN],[ECHO,SEARCH],[SUP_DLP],[POL_A],[G_A],A1)),
 ("team-b brings its own model and shares qwen too. A leaked AWS key is blocked before it ever reaches a model.",6500,snap([QWEN,LLAMA],[ECHO,SEARCH],[SUP_DLP,ANA],[POL_A,POL_B],[G_A,G_B],A2)),
 ("The platform team locks the shared model too: bairav model deploy ... --dlp-ref model-guard. DLP now runs inside qwen's own pod, so every caller is checked, sidecar or not.",6500,snap([QWEN_DLP,LLAMA],[ECHO,SEARCH],[SUP_DLP,ANA],[POL_A,POL_B,POL_S],[G_A,G_B],A2)),
 ("Tool calls get DLP too: team-a adds --dlp-ref to its Skills, and plugs in a remote CRM's MCP server through a gateway that holds its token. Arguments and results are scanned both ways.",7000,snap([QWEN_DLP,LLAMA],SKILLS_DLP,[SUP_CRM,ANA],[POL_A,POL_B,POL_S],[G_A,G_B],A2)),
 ("Time to try a bigger model: bairav model upgrade qwen --to Qwen/Qwen3-1.7B-GGUF --eval support.yaml. It loads next to the old one, which keeps serving.",5500,snap([QWEN_TRY,LLAMA],SKILLS_DLP,[SUP_CRM,ANA],[POL_A,POL_B,POL_S],[G_A,G_B],A2)),
 ("The eval gate runs the team's 12 checks. The new model scores 10/12 (it stopped returning valid JSON), below the 100% bar, so the upgrade rolls back on its own. Agents never noticed.",7000,snap([QWEN_DLP,LLAMA],SKILLS_DLP,[SUP_CRM,ANA],[POL_A,POL_B,POL_S],[G_A,G_B],A2)),
 ("Models, Skills, Agents, DLP and evals, on your own cluster. tech.bairavs.com/orch-engine",6000,snap([QWEN_DLP,LLAMA],SKILLS_DLP,[SUP_CRM,ANA],[POL_A,POL_B,POL_S],[G_A,G_B],A2)),
]
out=[{"caption":c,"durationMs":d,"snapshot":s} for c,d,s in frames]
import os
DEST=os.path.join(os.path.dirname(os.path.abspath(__file__)),"..","cli","internal","dashboard","assets","demo.json")
json.dump(out,open(DEST,"w"),indent=1)
print(len(out),"frames")

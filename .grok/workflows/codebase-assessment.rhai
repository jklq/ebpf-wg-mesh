let meta = #{
    name: "codebase-assessment",
    description: "Thorough multi-agent assessment of Functionality, UX/Usability, Security, Maintainability, and Performance/Efficiency",
    when_to_use: "Full-repo quality audit across product fitness, usability, security, maintainability, and performance",
    phases: [
        #{ title: "Map", detail: "Map architecture, intended behavior, and surface area" },
        #{ title: "Assess", detail: "Deep parallel assessment across five quality dimensions" },
        #{ title: "Cross-cut", detail: "Testing, docs, and end-to-end consistency" },
        #{ title: "Verify", detail: "Adversarial verification of critical findings" },
        #{ title: "Synthesize", detail: "Score, prioritize, and write the assessment report" },
    ],
};

// --- Schemas ---

let map_schema = #{
    "type": "object",
    "required": ["purpose", "components", "user_journeys", "trust_boundaries", "notes"],
    "properties": #{
        "purpose": #{ "type": "string" },
        "components": #{
            "type": "array",
            "maxItems": 20,
            "items": #{
                "type": "object",
                "required": ["name", "path", "role"],
                "properties": #{
                    "name": #{ "type": "string" },
                    "path": #{ "type": "string" },
                    "role": #{ "type": "string" },
                },
            },
        },
        "user_journeys": #{
            "type": "array",
            "maxItems": 12,
            "items": #{ "type": "string" },
        },
        "trust_boundaries": #{
            "type": "array",
            "maxItems": 12,
            "items": #{ "type": "string" },
        },
        "notes": #{ "type": "string" },
    },
};

let findings_schema = #{
    "type": "object",
    "required": ["dimension", "score", "summary", "strengths", "findings", "gaps"],
    "properties": #{
        "dimension": #{ "type": "string" },
        "score": #{ "type": "integer", "minimum": 1, "maximum": 10 },
        "summary": #{ "type": "string" },
        "strengths": #{
            "type": "array",
            "maxItems": 8,
            "items": #{ "type": "string" },
        },
        "findings": #{
            "type": "array",
            "maxItems": 10,
            "items": #{
                "type": "object",
                "required": ["id", "severity", "title", "detail", "evidence", "file", "recommendation"],
                "properties": #{
                    "id": #{ "type": "string" },
                    "severity": #{ "type": "string" },
                    "title": #{ "type": "string" },
                    "detail": #{ "type": "string" },
                    "evidence": #{ "type": "string" },
                    "file": #{ "type": "string" },
                    "recommendation": #{ "type": "string" },
                },
            },
        },
        "gaps": #{
            "type": "array",
            "maxItems": 8,
            "items": #{ "type": "string" },
        },
    },
};

let verdict_schema = #{
    "type": "object",
    "required": ["real", "severity_ok", "reason", "evidence"],
    "properties": #{
        "real": #{ "type": "boolean" },
        "severity_ok": #{ "type": "boolean" },
        "reason": #{ "type": "string" },
        "evidence": #{ "type": "string" },
        "adjusted_severity": #{ "type": "string" },
    },
};

let report_schema = #{
    "type": "object",
    "required": ["executive_summary", "scores", "top_risks", "top_strengths", "priority_actions", "report_markdown"],
    "properties": #{
        "executive_summary": #{ "type": "string" },
        "scores": #{
            "type": "object",
            "required": ["functionality", "ux_usability", "security", "maintainability", "performance", "overall"],
            "properties": #{
                "functionality": #{ "type": "integer", "minimum": 1, "maximum": 10 },
                "ux_usability": #{ "type": "integer", "minimum": 1, "maximum": 10 },
                "security": #{ "type": "integer", "minimum": 1, "maximum": 10 },
                "maintainability": #{ "type": "integer", "minimum": 1, "maximum": 10 },
                "performance": #{ "type": "integer", "minimum": 1, "maximum": 10 },
                "overall": #{ "type": "integer", "minimum": 1, "maximum": 10 },
            },
        },
        "top_risks": #{
            "type": "array",
            "maxItems": 12,
            "items": #{ "type": "string" },
        },
        "top_strengths": #{
            "type": "array",
            "maxItems": 10,
            "items": #{ "type": "string" },
        },
        "priority_actions": #{
            "type": "array",
            "maxItems": 15,
            "items": #{ "type": "string" },
        },
        "report_markdown": #{ "type": "string" },
    },
};

// Optional root scope; default is full repo.
let root = ".";
if args != () && args.root != () && type_of(args.root) == "string" && args.root != "" {
    root = args.root;
}

let max_verify = 24;
if args != () && args.max_verify != () {
    max_verify = args.max_verify;
}

// ============================================================================
// Phase 1: Map the system
// ============================================================================

phase("Map");
log("Mapping architecture, intended behavior, and trust boundaries for " + root);

let map_jobs = [];

let p0 = "";
p0 += "You are mapping the ebpf-wg-mesh codebase (a minimal PaaS control plane + agent + console with WireGuard/eBPF mesh). ";
p0 += "Root to inspect: " + root + ". ";
p0 += "Use ONLY read-only tools: list_dir, read_file, grep. Do NOT answer from memory. ";
p0 += "Read README.md, docs/, Makefile, go.mod, console/package.json, api/proto/*.proto, and skim cmd/* and internal/* layout. ";
p0 += "Identify major components (control plane, agent, console, mesh/firewall, builder, localteststack), each component's path and role, ";
p0 += "primary user journeys (bootstrap, enroll agent, deploy service, domains, GitHub link, auth), and trust boundaries (browser, console, control plane, agents, GitHub, Caddy, DB). ";
p0 += "Empty components/journeys are invalid unless you truly found nothing after reading those files — quote paths you opened. ";
p0 += "Return structured purpose, components, user_journeys, trust_boundaries, notes.";
map_jobs.push(#{
    prompt: p0,
    label: "map:architecture",
    capability_mode: "read-only",
    output_schema: map_schema,
});

let p1 = "";
p1 += "Map the INTENDED product capabilities of ebpf-wg-mesh by reading code and docs under " + root + ". ";
p1 += "Use read_file/grep/list_dir only. Read README.md, docs/, api/proto/platform.proto, api/proto/agent.proto, ";
p1 += "internal/controlplane/platform_service.go (and related), console/src/routes/, internal/agent/, internal/mesh/. ";
p1 += "List what the system is supposed to do end-to-end: auth, projects, services, deploys, domains, GitHub integration, mesh networking, ingress, managed dashboard, secrets (1Password), local test stack. ";
p1 += "Note explicit non-goals or prototype limitations from docs/code comments. ";
p1 += "Populate purpose (product intent), components (capability owners), user_journeys (happy paths), trust_boundaries, notes (gaps between docs and code). ";
p1 += "You MUST open files before answering.";
map_jobs.push(#{
    prompt: p1,
    label: "map:capabilities",
    capability_mode: "read-only",
    output_schema: map_schema,
});

let p2 = "";
p2 += "Map the operational and security surface of ebpf-wg-mesh under " + root + ". ";
p2 += "Use read-only tools. Inspect bootstrap (internal/bootstrap/), PKI (internal/controlplane/pki.go, internal_auth.go), ";
p2 += "console auth (console/src/lib/dashboard/core/auth.server.ts, jwt.server.ts, routes/auth/), ";
p2 += "webhooks, ingress, config validation, Makefile targets, infra/test-vm, localteststack. ";
p2 += "Document entry points, config surfaces, network ports/APIs, identity models, and how components authenticate to each other. ";
p2 += "Return structured map with purpose, components (entry points / identity surfaces), user_journeys (bootstrap/ops flows), trust_boundaries, notes.";
map_jobs.push(#{
    prompt: p2,
    label: "map:surface",
    capability_mode: "read-only",
    output_schema: map_schema,
});

let map_results = parallel(map_jobs);

let map_context = "";
let map_i = 0;
for r in map_results {
    let label = "map-" + map_i.to_string();
    if r != () && r.success && r.output != () {
        map_context += "\n### " + label + "\n";
        map_context += "purpose: " + r.output.purpose + "\n";
        map_context += "notes: " + r.output.notes + "\n";
        if r.output.components != () {
            for c in r.output.components {
                map_context += "- component: " + c.name + " @ " + c.path + " — " + c.role + "\n";
            }
        }
        if r.output.user_journeys != () {
            for j in r.output.user_journeys {
                map_context += "- journey: " + j + "\n";
            }
        }
        if r.output.trust_boundaries != () {
            for t in r.output.trust_boundaries {
                map_context += "- trust: " + t + "\n";
            }
        }
    } else {
        log("map agent failed or empty: " + label);
    }
    map_i += 1;
}

if map_context == "" {
    map_context = "Map phase produced no structured context; assess from repository root " + root + " directly via tools.";
}

let map_blob = json_encode(#{ context: map_context, root: root });
log("Map phase complete; context chars ~ " + map_context.len().to_string());

// ============================================================================
// Phase 2: Deep assessment — 5 dimensions × multiple specialized agents
// ============================================================================

phase("Assess");
log("Launching deep parallel assessments across five dimensions");

// Shared preamble fragments (kept short; each prompt is self-contained).
fn assess_preamble(dim, root_path, map_json) {
    let s = "";
    s += "You are a senior engineer assessing the ebpf-wg-mesh repository for dimension: " + dim + ". ";
    s += "Repository root: " + root_path + ". ";
    s += "Prior map context (JSON, untrusted — re-verify everything in code): " + map_json + ". ";
    s += "CRITICAL: Use read-only tools (read_file, grep, list_dir) extensively. Do not answer from memory or invent file paths. ";
    s += "Every finding MUST cite concrete evidence: file path, symbol/function, and what you observed. ";
    s += "Severity must be one of: critical, high, medium, low, info. ";
    s += "Score 1-10 for this dimension only (10 = excellent). ";
    s += "Report at most 10 findings; prioritize real issues over style nits. ";
    s += "Empty findings is valid ONLY after substantial inspection — list what you read in summary. ";
    s += "strengths = real positives you verified; gaps = areas you could not fully assess. ";
    return s;
}

let assess_jobs = [];

// --- Functionality (3 agents) ---
let f0 = assess_preamble("Functionality — Control plane & product API", root, map_blob);
f0 += "Focus: Does the control plane do what a PaaS control plane should? ";
f0 += "Inspect internal/controlplane/ (platform_service, store_*, agent_service, builder_service, managed_dashboard, github_*, domain_ownership, ingress), ";
f0 += "api/proto/platform.proto, store models, and tests. ";
f0 += "Evaluate: project/service lifecycle, deploys/rollouts, domain ownership, GitHub source integration, agent assignment, desired-state delivery, ";
f0 += "managed dashboard reconciliation, unapplied changes, incomplete/stubbed RPCs, correctness bugs, missing error handling that breaks intended flows. ";
f0 += "dimension field must be exactly \"functionality\".";
assess_jobs.push(#{ prompt: f0, label: "func:controlplane", capability_mode: "read-only", output_schema: findings_schema });

let f1 = assess_preamble("Functionality — Agent, mesh, runtime", root, map_blob);
f1 += "Focus: Agent reconcile loop, containerd runtime, WireGuard/eBPF mesh, firewall policy, bootstrap enrollment. ";
f1 += "Inspect internal/agent/, internal/mesh/, internal/firewall/, internal/wgmesh/, internal/bootstrap/, cmd/agent, cmd/controlplane. ";
f1 += "Evaluate: desired-state application, status reporting, mesh assignment correctness, platform stubs vs real Linux paths, failure recovery, ";
f1 += "whether agents stay dumb as designed, and whether runtime paths match docs. ";
f1 += "dimension field must be exactly \"functionality\".";
assess_jobs.push(#{ prompt: f1, label: "func:agent-mesh", capability_mode: "read-only", output_schema: findings_schema });

let f2 = assess_preamble("Functionality — Console product flows", root, map_blob);
f2 += "Focus: Console/dashboard product behavior — auth, onboarding, service canvas, deploys, domains, variables, GitHub picker, webhooks, SSE events. ";
f2 += "Inspect console/src/routes/, console/src/lib/dashboard/, console/src/lib/platform-grpc/, e2e and unit tests. ";
f2 += "Evaluate: whether UI flows call the right backend ops, incomplete screens, broken server-fns, auth callback correctness, ";
f2 += "onboarding completeness, and alignment with control-plane capabilities. ";
f2 += "dimension field must be exactly \"functionality\".";
assess_jobs.push(#{ prompt: f2, label: "func:console", capability_mode: "read-only", output_schema: findings_schema });

// --- UX / Usability (3 agents) ---
let u0 = assess_preamble("UX/Usability — Console UI", root, map_blob);
u0 += "Focus: Is the console easy and clear for a developer deploying services? ";
u0 += "Inspect console/src/routes/-dashboard/ (dashboard-page, service-panel, topbar, modals, panels), styles, empty states, error presentation, ";
u0 += "loading states, copy/labels, navigation, accessibility basics in markup. ";
u0 += "Evaluate: cognitive load, feedback on long ops (deploy/build), error messages, discoverability of domains/variables/GitHub link, dead ends. ";
u0 += "dimension field must be exactly \"ux_usability\".";
assess_jobs.push(#{ prompt: u0, label: "ux:console-ui", capability_mode: "read-only", output_schema: findings_schema });

let u1 = assess_preamble("UX/Usability — Developer experience & ops", root, map_blob);
u1 += "Focus: DX for operators and contributors. ";
u1 += "Inspect README.md, Makefile, docs/, cmd/localteststack/, bootstrap flags/env in internal/bootstrap and config, error messages from validate.go, ";
u1 += "scripts/, infra/test-vm, console package.json scripts. ";
u1 += "Evaluate: time-to-first-success, clarity of bootstrap, local stack ergonomics, test commands, config discoverability, missing docs, footguns. ";
u1 += "dimension field must be exactly \"ux_usability\".";
assess_jobs.push(#{ prompt: u1, label: "ux:dx-ops", capability_mode: "read-only", output_schema: findings_schema });

let u2 = assess_preamble("UX/Usability — API & integration clarity", root, map_blob);
u2 += "Focus: Clarity of platform/agent APIs and integration surfaces for clients. ";
u2 += "Inspect api/proto/*.proto, convert.go, platform-grpc gateway/client, webhook handlers, event streams (console events routes). ";
u2 += "Evaluate: naming consistency, error model, idempotency, pagination, status enums, documentation of RPCs, surprising behaviors. ";
u2 += "dimension field must be exactly \"ux_usability\".";
assess_jobs.push(#{ prompt: u2, label: "ux:api", capability_mode: "read-only", output_schema: findings_schema });

// --- Security (4 agents) ---
let s0 = assess_preamble("Security — Authentication & authorization", root, map_blob);
s0 += "Focus: Authn/authz correctness. ";
s0 += "Inspect console auth/JWT/session (console/src/lib/dashboard/core/), control plane internal_auth, platform_service authz checks, ";
s0 += "project membership/roles in store, bootstrap tokens, agent enrollment identity, dashboard identity (internal/agent/dashboard_identity.go). ";
s0 += "Look for: missing authz on RPCs, IDOR, privilege escalation, session fixation, JWT misuse, token leakage, confused deputy between console and CP. ";
s0 += "dimension field must be exactly \"security\".";
assess_jobs.push(#{ prompt: s0, label: "sec:authz", capability_mode: "read-only", output_schema: findings_schema });

let s1 = assess_preamble("Security — Transport, PKI, secrets", root, map_blob);
s1 += "Focus: mTLS, PKI lifecycle, secret handling, WireGuard keys. ";
s1 += "Inspect internal/controlplane/pki.go, internal/agent/tls.go, bootstrap cert issuance, cmd/internal-client-cert, ";
s1 += "1Password integration, env loading, store of tokens/keys, logging of secrets. ";
s1 += "Look for: weak TLS config, cert validation gaps, key material on disk without protection, secrets in logs/proto, bootstrap token reuse, CA trust issues. ";
s1 += "dimension field must be exactly \"security\".";
assess_jobs.push(#{ prompt: s1, label: "sec:pki-secrets", capability_mode: "read-only", output_schema: findings_schema });

let s2 = assess_preamble("Security — Attack surface & input safety", root, map_blob);
s2 += "Focus: External attack surface and input validation. ";
s2 += "Inspect GitHub webhooks (signature verification), domain DNS challenge, source archive/inspection, Caddy config generation, ";
s2 += "SQL usage in store (injection?), path traversal in source/work dirs, SSRF, command execution in builder, console server-fns inputs. ";
s2 += "Look for: unvalidated hostnames, shell injection, webhook spoofing, unsafe deserialization, overly broad CORS, debug endpoints. ";
s2 += "dimension field must be exactly \"security\".";
assess_jobs.push(#{ prompt: s2, label: "sec:surface", capability_mode: "read-only", output_schema: findings_schema });

let s3 = assess_preamble("Security — Multi-tenant isolation & mesh policy", root, map_blob);
s3 += "Focus: Tenant isolation and network policy. ";
s3 += "Inspect store project scoping, service allocation, eBPF firewall maps/policy, mesh assignment, volume mounts, registry policy, ";
s3 += "managed dashboard isolation, agent desired-state scope (can an agent see other tenants?). ";
s3 += "Look for: cross-project data leaks, shared mesh without isolation, overly permissive firewall defaults, missing row-level filters. ";
s3 += "dimension field must be exactly \"security\".";
assess_jobs.push(#{ prompt: s3, label: "sec:isolation", capability_mode: "read-only", output_schema: findings_schema });

// --- Maintainability (3 agents) ---
let m0 = assess_preamble("Maintainability — Structure & modularity", root, map_blob);
m0 += "Focus: Code organization, package boundaries, coupling, consistency. ";
m0 += "Inspect internal/controlplane package size/split, console lib layering, generated vs hand-written code, config types, convert layers. ";
m0 += "Evaluate: god packages, circular deps, unclear ownership, naming consistency, duplication, proto/versioning hygiene, worktree/local drift. ";
m0 += "dimension field must be exactly \"maintainability\".";
assess_jobs.push(#{ prompt: m0, label: "maint:structure", capability_mode: "read-only", output_schema: findings_schema });

let m1 = assess_preamble("Maintainability — Tests & change safety", root, map_blob);
m1 += "Focus: Test coverage quality and safety nets for change. ";
m1 += "Inspect *_test.go across internal/, console unit/e2e tests, Makefile test targets, integration harnesses, testkit. ";
m1 += "Evaluate: critical paths untested, brittle tests, missing authz tests, flaky patterns, integration vs unit balance, fixtures quality, ";
m1 += "whether a new contributor can refactor safely. ";
m1 += "dimension field must be exactly \"maintainability\".";
assess_jobs.push(#{ prompt: m1, label: "maint:tests", capability_mode: "read-only", output_schema: findings_schema });

let m2 = assess_preamble("Maintainability — Complexity & tech debt", root, map_blob);
m2 += "Focus: Complexity hotspots and debt that slow future work. ";
m2 += "Search for TODO/FIXME/HACK, large functions, deep nesting, platform-specific stubs, deleted auth modules vs replacements, ";
m2 += "dual schemas (console DB vs CP DB), migration patterns. ";
m2 += "Evaluate: cognitive load of key files (store_services*, platform_service, managed_dashboard, runtime), upgrade risks, observability for debugging. ";
m2 += "dimension field must be exactly \"maintainability\".";
assess_jobs.push(#{ prompt: m2, label: "maint:debt", capability_mode: "read-only", output_schema: findings_schema });

// --- Performance / Efficiency (3 agents) ---
let e0 = assess_preamble("Performance/Efficiency — Control plane & data plane", root, map_blob);
e0 += "Focus: Control plane hot paths and DB usage. ";
e0 += "Inspect store queries, N+1 patterns, full desired-state snapshot size for agents, poll/stream loops, log store, reconciler intervals, ";
e0 += "GitHub reconciler, Caddy full-config replace frequency. ";
e0 += "Look for: unbounded lists, missing indexes (schema), chatty gRPC, lock contention, inefficient JSON/proto conversion, O(n^2) assignment. ";
e0 += "dimension field must be exactly \"performance\".";
assess_jobs.push(#{ prompt: e0, label: "perf:controlplane", capability_mode: "read-only", output_schema: findings_schema });

let e1 = assess_preamble("Performance/Efficiency — Agent & mesh runtime", root, map_blob);
e1 += "Focus: Agent reconcile cost, containerd interactions, eBPF/WireGuard path efficiency. ";
e1 += "Inspect internal/agent/runtime*.go, mesh runtime, firewall manager, log shipping. ";
e1 += "Look for: full reconcile thrash, expensive periodic work, memory leaks patterns, blocking calls on hot paths, unnecessary restarts. ";
e1 += "dimension field must be exactly \"performance\".";
assess_jobs.push(#{ prompt: e1, label: "perf:agent", capability_mode: "read-only", output_schema: findings_schema });

let e2 = assess_preamble("Performance/Efficiency — Console & frontend", root, map_blob);
e2 += "Focus: Console SSR/client efficiency and API chattiness. ";
e2 += "Inspect console routes, server-fns, platform-grpc client, events/SSE routes, dashboard data loading, vite config. ";
e2 += "Look for: waterfalls, over-fetching, missing caching, large client bundles cues, polling vs events, expensive re-renders patterns in React. ";
e2 += "dimension field must be exactly \"performance\".";
assess_jobs.push(#{ prompt: e2, label: "perf:console", capability_mode: "read-only", output_schema: findings_schema });

log("Assess panel size: " + assess_jobs.len().to_string() + " agents");
let assess_results = parallel(assess_jobs);

// Collect findings with provenance
let all_findings = [];
let dimension_summaries = [];
let score_func = [];
let score_ux = [];
let score_sec = [];
let score_maint = [];
let score_perf = [];

let ai = 0;
for r in assess_results {
    if r != () && r.success && r.output != () {
        let out = r.output;
        let dim = "";
        if out.dimension != () { dim = out.dimension; }
        let score = 0;
        if out.score != () { score = out.score; }

        if dim == "functionality" { score_func.push(score); }
        if dim == "ux_usability" { score_ux.push(score); }
        if dim == "security" { score_sec.push(score); }
        if dim == "maintainability" { score_maint.push(score); }
        if dim == "performance" { score_perf.push(score); }

        let summ = #{
            dimension: dim,
            score: score,
            summary: out.summary,
            strengths: out.strengths,
            gaps: out.gaps,
            agent_index: ai,
        };
        dimension_summaries.push(summ);

        if out.findings != () {
            for f in out.findings {
                let item = #{
                    dimension: dim,
                    id: f.id,
                    severity: f.severity,
                    title: f.title,
                    detail: f.detail,
                    evidence: f.evidence,
                    file: f.file,
                    recommendation: f.recommendation,
                    source_agent: ai,
                };
                all_findings.push(item);
            }
        }
    } else {
        log("assess agent failed slot " + ai.to_string());
    }
    ai += 1;
}

log("Collected " + all_findings.len().to_string() + " raw findings from " + dimension_summaries.len().to_string() + " successful assessors");

// ============================================================================
// Phase 3: Cross-cutting assessment
// ============================================================================

phase("Cross-cut");
log("Cross-cutting: testing coherence, docs fidelity, E2E consistency");

let x_jobs = [];

let x0 = assess_preamble("Functionality — End-to-end consistency", root, map_blob);
x0 += "Cross-cut: Trace 2-3 critical journeys across console → platform-grpc → control plane → agent → mesh/ingress. ";
x0 += "Journeys: (1) OAuth login to project ready, (2) create service from GitHub and deploy, (3) custom domain bind. ";
x0 += "Find broken links, dual sources of truth, missing handoffs, status that never updates. ";
x0 += "dimension must be \"functionality\".";
x_jobs.push(#{ prompt: x0, label: "x:e2e-journeys", capability_mode: "read-only", output_schema: findings_schema });

let x1 = assess_preamble("Maintainability — Documentation fidelity", root, map_blob);
x1 += "Cross-cut: Compare README.md and docs/ to actual code paths and flags. ";
x1 += "Flag outdated claims, missing setup steps, architecture drift, deleted modules still documented, unmentioned critical config. ";
x1 += "dimension must be \"maintainability\".";
x_jobs.push(#{ prompt: x1, label: "x:docs", capability_mode: "read-only", output_schema: findings_schema });

let x2 = assess_preamble("Security — Defense in depth review", root, map_blob);
x2 += "Cross-cut: Assume one boundary fails (stolen console session, compromised agent, malicious GitHub payload). ";
x2 += "What blast radius remains? Read auth, mTLS identity separation, store scoping, webhook verify, bootstrap single-use tokens. ";
x2 += "dimension must be \"security\".";
x_jobs.push(#{ prompt: x2, label: "x:defense", capability_mode: "read-only", output_schema: findings_schema });

let x3 = assess_preamble("Performance/Efficiency — Scalability limits", root, map_blob);
x3 += "Cross-cut: Estimate scalability bottlenecks for N projects, M services/node, A agents. ";
x3 += "Read assignment algorithms, snapshot fan-out, DB transaction patterns, Caddy sync, console SSE. ";
x3 += "State concrete limits or O-notation with file evidence. dimension must be \"performance\".";
x_jobs.push(#{ prompt: x3, label: "x:scale", capability_mode: "read-only", output_schema: findings_schema });

let x_results = parallel(x_jobs);
let xi = 0;
for r in x_results {
    if r != () && r.success && r.output != () {
        let out = r.output;
        let dim = "";
        if out.dimension != () { dim = out.dimension; }
        let score = 0;
        if out.score != () { score = out.score; }
        if dim == "functionality" { score_func.push(score); }
        if dim == "ux_usability" { score_ux.push(score); }
        if dim == "security" { score_sec.push(score); }
        if dim == "maintainability" { score_maint.push(score); }
        if dim == "performance" { score_perf.push(score); }
        dimension_summaries.push(#{
            dimension: dim,
            score: score,
            summary: out.summary,
            strengths: out.strengths,
            gaps: out.gaps,
            agent_index: 100 + xi,
        });
        if out.findings != () {
            for f in out.findings {
                all_findings.push(#{
                    dimension: dim,
                    id: f.id,
                    severity: f.severity,
                    title: f.title,
                    detail: f.detail,
                    evidence: f.evidence,
                    file: f.file,
                    recommendation: f.recommendation,
                    source_agent: 100 + xi,
                });
            }
        }
    } else {
        log("cross-cut agent failed slot " + xi.to_string());
    }
    xi += 1;
}

log("After cross-cut: " + all_findings.len().to_string() + " total findings");

// ============================================================================
// Phase 4: Adversarial verification of highest-severity findings
// ============================================================================

phase("Verify");

// Severity rank helper
fn sev_rank(s) {
    if s == "critical" { return 0; }
    if s == "high" { return 1; }
    if s == "medium" { return 2; }
    if s == "low" { return 3; }
    return 4;
}

// Simple selection: take up to max_verify findings, preferring critical/high, stable order otherwise.
// Rhai has no sort, so multi-pass by severity.
let to_verify = [];
let severities = ["critical", "high", "medium", "low", "info"];
for sev in severities {
    if to_verify.len() >= max_verify { break; }
    for f in all_findings {
        if to_verify.len() >= max_verify { break; }
        if f.severity == sev {
            // de-dupe by title+file
            let dup = false;
            for t in to_verify {
                if t.title == f.title && t.file == f.file { dup = true; break; }
            }
            if !dup {
                to_verify.push(f);
            }
        }
    }
}

log("Verifying " + to_verify.len().to_string() + " findings (cap " + max_verify.to_string() + ")");

let vjobs = [];
let vi = 0;
for f in to_verify {
    let fp = json_encode(f);
    let vp = "";
    vp += "You are an adversarial skeptic. Independently verify this codebase assessment finding. ";
    vp += "Repository root: " + root + ". ";
    vp += "Finding JSON: " + fp + ". ";
    vp += "Use read_file/grep/list_dir on the cited file and related code. Do NOT trust the finding's evidence field — re-read the code. ";
    vp += "Set real=true ONLY if you find concrete confirming evidence in the shipped code. ";
    vp += "If the issue is theoretical, already mitigated, based on wrong path, or severity is inflated, set real=false or severity_ok=false. ";
    vp += "If real but severity wrong, set severity_ok=false and adjusted_severity to critical|high|medium|low|info. ";
    vp += "evidence must quote what you saw (function names, conditions, missing checks). Default real=false when unsure.";
    vjobs.push(#{
        prompt: vp,
        label: "verify:" + vi.to_string(),
        capability_mode: "read-only",
        output_schema: verdict_schema,
    });
    vi += 1;
}

let confirmed = [];
let rejected = [];
let adjusted = [];

if vjobs.len() == 0 {
    log("No findings to verify");
} else {
    let verdicts = parallel(vjobs);
    let vj = 0;
    for v in verdicts {
        let base = to_verify[vj];
        if v != () && v.success && v.output != () {
            if v.output.real == true {
                let sev = base.severity;
                if v.output.severity_ok == false && v.output.adjusted_severity != () && v.output.adjusted_severity != "" {
                    sev = v.output.adjusted_severity;
                    adjusted.push(base.title);
                }
                confirmed.push(#{
                    dimension: base.dimension,
                    id: base.id,
                    severity: sev,
                    title: base.title,
                    detail: base.detail,
                    evidence: v.output.evidence,
                    file: base.file,
                    recommendation: base.recommendation,
                    verify_reason: v.output.reason,
                });
            } else {
                rejected.push(#{
                    title: base.title,
                    file: base.file,
                    reason: v.output.reason,
                });
            }
        } else {
            // Fail closed: unverified does not count as confirmed
            rejected.push(#{
                title: base.title,
                file: base.file,
                reason: "verification agent failed or returned unusable output",
            });
        }
        vj += 1;
    }
}

log(confirmed.len().to_string() + " confirmed, " + rejected.len().to_string() + " rejected/unverified");

// ============================================================================
// Phase 5: Synthesize report
// ============================================================================

phase("Synthesize");

fn avg_score(scores) {
    if scores.len() == 0 { return 5; }
    let sum = 0;
    for s in scores { sum += s; }
    // integer average
    return sum / scores.len();
}

let scores_obj = #{
    functionality: avg_score(score_func),
    ux_usability: avg_score(score_ux),
    security: avg_score(score_sec),
    maintainability: avg_score(score_maint),
    performance: avg_score(score_perf),
};
let overall = (scores_obj.functionality + scores_obj.ux_usability + scores_obj.security + scores_obj.maintainability + scores_obj.performance) / 5;
scores_obj.overall = overall;

// Build condensed packet for synthesizer (bound size)
let packet = #{
    root: root,
    scores: scores_obj,
    map_context: map_context.sub_string(0, if map_context.len() > 6000 { 6000 } else { map_context.len() }),
    dimension_summaries: [],
    confirmed_findings: confirmed,
    rejected_count: rejected.len(),
    raw_finding_count: all_findings.len(),
    verified_count: confirmed.len(),
};

let di = 0;
for d in dimension_summaries {
    if di >= 24 { break; }
    let st = [];
    if d.strengths != () {
        let si = 0;
        for s in d.strengths {
            if si >= 4 { break; }
            st.push(s);
            si += 1;
        }
    }
    packet.dimension_summaries.push(#{
        dimension: d.dimension,
        score: d.score,
        summary: d.summary,
        strengths: st,
    });
    di += 1;
}

let packet_json = json_encode(packet);

let syn = "";
syn += "You are writing the FINAL comprehensive assessment report for the ebpf-wg-mesh codebase. ";
syn += "Use the structured assessment packet below. You MAY use read_file/grep lightly to clarify a citation, but do not re-litigate rejected findings. ";
syn += "Packet JSON: " + packet_json + ". ";
syn += "Write report_markdown as a full professional report with these sections exactly:\n";
syn += "1. Executive summary\n";
syn += "2. Scorecard (table of the five dimensions + overall, with one-line rationale each)\n";
syn += "3. Functionality assessment\n";
syn += "4. UX/Usability assessment\n";
syn += "5. Security assessment\n";
syn += "6. Maintainability assessment\n";
syn += "7. Performance/Efficiency assessment\n";
syn += "8. Confirmed findings (grouped by severity, each with file, evidence, recommendation)\n";
syn += "9. Strengths to preserve\n";
syn += "10. Priority action plan (ordered, concrete, size S/M/L)\n";
syn += "11. Assessment method & limits (note how many agents, verified vs raw counts)\n";
syn += "Scores in the scores object should match the packet averages unless a clear reason to adjust by at most 1 point. ";
syn += "top_risks and top_strengths and priority_actions must be non-empty. ";
syn += "executive_summary: 1 short paragraph. report_markdown: the full report body.";

let syn_result = agent(syn, #{
    label: "synthesize:report",
    capability_mode: "read-only",
    output_schema: report_schema,
});

let report_md = "";
let final_scores = scores_obj;
let exec_sum = "";
let top_risks = [];
let top_strengths = [];
let priority_actions = [];

if syn_result != () && syn_result.success && syn_result.output != () {
    let o = syn_result.output;
    if o.report_markdown != () { report_md = o.report_markdown; }
    if o.executive_summary != () { exec_sum = o.executive_summary; }
    if o.scores != () { final_scores = o.scores; }
    if o.top_risks != () { top_risks = o.top_risks; }
    if o.top_strengths != () { top_strengths = o.top_strengths; }
    if o.priority_actions != () { priority_actions = o.priority_actions; }
} else {
    log("Synthesizer failed; building fallback report from confirmed findings");
    report_md = "# Codebase Assessment (fallback)\n\n";
    report_md += "Synthesizer agent failed. Confirmed findings only.\n\n";
    report_md += "## Scores (averages)\n\n";
    report_md += "- Functionality: " + scores_obj.functionality.to_string() + "\n";
    report_md += "- UX/Usability: " + scores_obj.ux_usability.to_string() + "\n";
    report_md += "- Security: " + scores_obj.security.to_string() + "\n";
    report_md += "- Maintainability: " + scores_obj.maintainability.to_string() + "\n";
    report_md += "- Performance: " + scores_obj.performance.to_string() + "\n";
    report_md += "- Overall: " + scores_obj.overall.to_string() + "\n\n";
    report_md += "## Confirmed findings\n\n";
    for f in confirmed {
        report_md += "### [" + f.severity + "] " + f.title + "\n";
        report_md += "- Dimension: " + f.dimension + "\n";
        report_md += "- File: " + f.file + "\n";
        report_md += "- Detail: " + f.detail + "\n";
        report_md += "- Evidence: " + f.evidence + "\n";
        report_md += "- Recommendation: " + f.recommendation + "\n\n";
    }
    exec_sum = "Fallback report: synthesizer unavailable; " + confirmed.len().to_string() + " confirmed findings.";
}

// Prepend metadata header
let header = "";
header += "---\n";
header += "title: ebpf-wg-mesh Codebase Assessment\n";
header += "workflow: codebase-assessment\n";
header += "root: " + root + "\n";
header += "raw_findings: " + all_findings.len().to_string() + "\n";
header += "verified_confirmed: " + confirmed.len().to_string() + "\n";
header += "rejected_or_unverified: " + rejected.len().to_string() + "\n";
header += "assess_agents: 16 dimension + 4 cross-cut + 3 map + N verify + 1 synth\n";
header += "---\n\n";

let full_report = header + report_md;

// Append rejected summary for transparency
full_report += "\n\n## Verification appendix\n\n";
full_report += "Rejected or unverified candidates: " + rejected.len().to_string() + "\n\n";
let ri = 0;
for r in rejected {
    if ri >= 30 {
        full_report += "\n... truncated ...\n";
        break;
    }
    full_report += "- ~~" + r.title + "~~ (" + r.file + "): " + r.reason + "\n";
    ri += 1;
}

let report_path = write_scratch_file("codebase-assessment-report.md", full_report);
log("Report written to " + report_path);

complete(#{
    path: report_path,
    executive_summary: exec_sum,
    scores: final_scores,
    top_risks: top_risks,
    top_strengths: top_strengths,
    priority_actions: priority_actions,
    stats: #{
        raw_findings: all_findings.len(),
        confirmed: confirmed.len(),
        rejected: rejected.len(),
        map_agents: 3,
        assess_agents: 16,
        crosscut_agents: 4,
        verify_agents: to_verify.len(),
        synth_agents: 1,
    },
    report: full_report,
});

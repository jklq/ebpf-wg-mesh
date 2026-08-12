let meta = #{
    name: "update-tests",
    description: "After a code change, discover outdated tests/harnesses across all surfaces in parallel, fix small subsets with shared change context, then re-verify",
    when_to_use: "Tests passed before a recent product/code change; update tests and harnesses (not revert product code) across Go unit/integration, console unit/integration/e2e-local, and e2e-vm (static)",
    phases: [
        #{ title: "Context", detail: "Summarize intentional code change vs base" },
        #{ title: "Discover", detail: "Run surfaces in parallel; static-analyze e2e-vm" },
        #{ title: "Fix", detail: "One agent per small test unit with global change context" },
        #{ title: "Verify", detail: "Re-run only surfaces that had failures" },
        #{ title: "Report", detail: "Write outcome report" },
    ],
};

// --- Schemas ---

let context_schema = #{
    "type": "object",
    "required": ["summary", "changed_areas", "api_behavior_changes", "test_impact_hints"],
    "properties": #{
        "summary": #{ "type": "string" },
        "changed_areas": #{
            "type": "array",
            "maxItems": 40,
            "items": #{ "type": "string" },
        },
        "api_behavior_changes": #{
            "type": "array",
            "maxItems": 30,
            "items": #{ "type": "string" },
        },
        "test_impact_hints": #{
            "type": "array",
            "maxItems": 30,
            "items": #{ "type": "string" },
        },
        "base_ref": #{ "type": "string" },
    },
};

let discovery_schema = #{
    "type": "object",
    "required": ["surface", "passed", "failures", "summary"],
    "properties": #{
        "surface": #{ "type": "string" },
        "passed": #{ "type": "boolean" },
        "command": #{ "type": "string" },
        "summary": #{ "type": "string" },
        "failures": #{
            "type": "array",
            "maxItems": 50,
            "items": #{
                "type": "object",
                "required": ["id", "path", "title", "detail", "kind"],
                "properties": #{
                    "id": #{ "type": "string" },
                    "path": #{ "type": "string" },
                    "pkg": #{ "type": "string" },
                    "test_name": #{ "type": "string" },
                    "title": #{ "type": "string" },
                    "detail": #{ "type": "string" },
                    "kind": #{ "type": "string" },
                },
            },
        },
        "harness_paths": #{
            "type": "array",
            "maxItems": 30,
            "items": #{ "type": "string" },
        },
    },
};

let fix_schema = #{
    "type": "object",
    "required": ["unit_id", "status", "files_changed", "notes"],
    "properties": #{
        "unit_id": #{ "type": "string" },
        "status": #{ "type": "string" },
        "files_changed": #{
            "type": "array",
            "maxItems": 30,
            "items": #{ "type": "string" },
        },
        "notes": #{ "type": "string" },
        "verify_command": #{ "type": "string" },
        "verify_passed": #{ "type": "boolean" },
        "remaining": #{
            "type": "array",
            "maxItems": 15,
            "items": #{ "type": "string" },
        },
    },
};

let verify_schema = #{
    "type": "object",
    "required": ["surface", "passed", "summary"],
    "properties": #{
        "surface": #{ "type": "string" },
        "passed": #{ "type": "boolean" },
        "summary": #{ "type": "string" },
        "remaining_failures": #{
            "type": "array",
            "maxItems": 40,
            "items": #{ "type": "string" },
        },
    },
};

// --- Helpers ---

fn starts_with(s, pref) {
    if type_of(s) != "string" || type_of(pref) != "string" {
        return false;
    }
    if pref.len() > s.len() {
        return false;
    }
    return s.sub_string(0, pref.len()) == pref;
}

fn path_allowed(p) {
    if type_of(p) != "string" || p == "" {
        return false;
    }
    // Reject obvious escapes; keep scope inside this monorepo.
    let i = 0;
    let n = p.len();
    while i + 1 < n {
        if p.sub_string(i, 2) == ".." {
            return false;
        }
        i += 1;
    }
    let prefixes = [
        "internal/", "cmd/", "console/", "api/", "scripts/", "infra/",
        "docs/", "tests/", "./internal/", "./cmd/", "./console/", "./api/",
        "./scripts/", "./infra/", "console/src/", "console/tests/",
        "Makefile", "go.mod", "go.sum", ".env.example",
    ];
    for pref in prefixes {
        if starts_with(p, pref) {
            return true;
        }
    }
    // Go package paths sometimes reported as "./..."
    if starts_with(p, "./") {
        return true;
    }
    // Bare package dirs like "internal/controlplane"
    if starts_with(p, "internal") || starts_with(p, "cmd") || starts_with(p, "console") {
        return true;
    }
    return false;
}

fn surface_enabled(wanted, name) {
    if wanted.len() == 0 {
        return true;
    }
    for w in wanted {
        if w == name {
            return true;
        }
    }
    return false;
}

fn surface_command(name) {
    if name == "unit-go" {
        return "make test-unit-go";
    }
    if name == "unit-console" {
        return "make test-unit-console";
    }
    if name == "integration-go" {
        return "make test-integration-go";
    }
    if name == "integration-console" {
        return "make test-integration-console";
    }
    if name == "e2e-local" {
        return "make test-e2e-local";
    }
    if name == "e2e-vm" {
        return "(static analysis only — do NOT run make test-e2e-vm)";
    }
    return "";
}

fn unit_key(surface, path) {
    return surface + "|" + path;
}

// --- Args ---

let base_ref = "main";
if args != () && args.base != () && type_of(args.base) == "string" && args.base != "" {
    base_ref = args.base;
}

let max_units = 20;
if args != () && args.max_units != () {
    max_units = args.max_units;
}
if max_units < 1 {
    max_units = 1;
}
if max_units > 48 {
    max_units = 48;
}

// Optional surface filter: array of names. Empty/absent = all.
let wanted_surfaces = [];
if args != () && args.surfaces != () {
    for s in args.surfaces {
        if type_of(s) == "string" && s != "" {
            wanted_surfaces.push(s);
        }
    }
}

// include_slow defaults true (integration + e2e-local). Set false to skip heavy surfaces.
let include_slow = true;
if args != () && args.include_slow != () && args.include_slow == false {
    include_slow = false;
}

let all_surfaces = ["unit-go", "unit-console", "integration-go", "integration-console", "e2e-local", "e2e-vm"];
if !include_slow {
    all_surfaces = ["unit-go", "unit-console", "e2e-vm"];
}

let active_surfaces = [];
for s in all_surfaces {
    if surface_enabled(wanted_surfaces, s) {
        active_surfaces.push(s);
    }
}
if active_surfaces.len() == 0 {
    pause("verification", "No active surfaces. Pass args.surfaces as a non-empty array of: unit-go, unit-console, integration-go, integration-console, e2e-local, e2e-vm — or omit surfaces to use defaults.");
}

log("update-tests base=" + base_ref + " max_units=" + max_units.to_string() + " surfaces=" + active_surfaces.len().to_string());

// ============================================================================
// Phase 1: Context — what intentionally changed
// ============================================================================

phase("Context");

let diff_text = git_diff_since(base_ref);
let diff_chars = 0;
if type_of(diff_text) == "string" {
    diff_chars = diff_text.len();
}
log("git_diff_since(" + base_ref + ") chars=" + diff_chars.to_string());

// Bound prompt size: agents get a truncated diff plus instructions to re-read files.
let diff_for_prompt = diff_text;
let diff_cap = 120000;
if type_of(diff_for_prompt) == "string" && diff_for_prompt.len() > diff_cap {
    diff_for_prompt = diff_for_prompt.sub_string(0, diff_cap);
    log("Diff truncated for context prompt to " + diff_cap.to_string() + " chars");
}
if type_of(diff_for_prompt) != "string" || diff_for_prompt == "" {
    diff_for_prompt = "(empty or unavailable diff vs " + base_ref + " — use git status / git diff / read changed files yourself)";
}

let ctx_prompt = "";
ctx_prompt += "You are preparing global change context for a test-update workflow on the ebpf-wg-mesh monorepo. ";
ctx_prompt += "Assumption: all tests PASSED on the pre-change baseline. The product/code change is intentional. ";
ctx_prompt += "Your job is NOT to fix tests yet — only summarize what changed and how tests/harnesses likely need to adapt. ";
ctx_prompt += "Base ref: " + base_ref + ". ";
ctx_prompt += "Use read-only tools (read_file, grep, list_dir, and shell git if available) to verify important hunks — do not answer from memory. ";
ctx_prompt += "Read the diff below, then open the most important changed files to understand new behavior, APIs, types, protos, routes, and config. ";
ctx_prompt += "Populate: summary (2-6 sentences), changed_areas (packages/paths), api_behavior_changes (observable contract changes), ";
ctx_prompt += "test_impact_hints (which surfaces and harnesses likely break: unit-go, unit-console, integration-*, e2e-local, e2e-vm/static). ";
ctx_prompt += "base_ref must be \"" + base_ref + "\". ";
ctx_prompt += "\n\n--- DIFF vs " + base_ref + " ---\n";
ctx_prompt += diff_for_prompt;

let ctx_result = agent(ctx_prompt, #{
    label: "context:change",
    capability_mode: "read-only",
    output_schema: context_schema,
});

let change_summary = "Change context unavailable; agents must derive intent from git diff vs " + base_ref + " and the code.";
let change_areas = [];
let api_changes = [];
let impact_hints = [];
if ctx_result != () && ctx_result.success && ctx_result.output != () {
    if ctx_result.output.summary != () && type_of(ctx_result.output.summary) == "string" {
        change_summary = ctx_result.output.summary;
    }
    if ctx_result.output.changed_areas != () {
        change_areas = ctx_result.output.changed_areas;
    }
    if ctx_result.output.api_behavior_changes != () {
        api_changes = ctx_result.output.api_behavior_changes;
    }
    if ctx_result.output.test_impact_hints != () {
        impact_hints = ctx_result.output.test_impact_hints;
    }
} else {
    log("Context agent failed or empty; continuing with raw diff only");
}

let change_blob = json_encode(#{
    base_ref: base_ref,
    summary: change_summary,
    changed_areas: change_areas,
    api_behavior_changes: api_changes,
    test_impact_hints: impact_hints,
});
log("Context ready");

// ============================================================================
// Phase 2: Discover outdated tests
// ============================================================================

phase("Discover");
log("Discovering outdated tests across " + active_surfaces.len().to_string() + " surface(s)");

let discover_jobs = [];
for s in active_surfaces {
    let cmd = surface_command(s);
    let p = "";
    p += "You are discovering outdated tests after an intentional product/code change in ebpf-wg-mesh. ";
    p += "Tests PASSED on the pre-change baseline. Do NOT revert product code. ";
    p += "Surface: " + s + ". ";
    p += "Global change context (JSON, re-verify in code): " + change_blob + ". ";
    p += "Base ref: " + base_ref + ". ";

    if s == "e2e-vm" {
        p += "IMPORTANT: Do NOT run `make test-e2e-vm` or provision VMs. Use STATIC ANALYSIS only. ";
        p += "Read and compare: cmd/testvm/, infra/test-vm/, related helpers in internal/localteststack/, internal/testutil/ if present, ";
        p += "and any docs/scripts that wire VM e2e. Cross-check against the changed product APIs/behavior (protos, controlplane, agent, bootstrap, console contracts). ";
        p += "Use read_file/grep/list_dir extensively. Report each outdated scenario, assertion, env/token, identity, flag, or harness assumption as a failure. ";
        p += "path must be a concrete file path under cmd/testvm, infra/test-vm, or a tightly related harness file. ";
        p += "kind must be one of: test, harness, assertion, compile, infra, missing. ";
        p += "If after real inspection the VM e2e harness still matches the new behavior, set passed=true and failures=[]. ";
        p += "Empty failures is valid ONLY after you opened the relevant files — mention them in summary. ";
    } else {
        p += "RUN the test surface now via shell: `" + cmd + "` from the repository root. ";
        p += "Capture full relevant failure output. You may re-run a narrower package/file command to localize failures after the suite fails. ";
        p += "Also note harness files if failures stem from testkit/helpers (console testkit, localteststack, go test helpers), not only the *_test.go / *.test.ts file. ";
        p += "Map each failure to: id (stable short id), path (primary file or package directory to own when fixing), pkg (Go package path if any), test_name, title, detail (include key error lines), kind. ";
        p += "kind must be one of: test, harness, assertion, compile, infra, missing. ";
        p += "Use kind=infra for environment/docker/network/setup failures that are NOT outdated assertions from the code change. ";
        p += "Use kind=compile for build failures in tests or packages under test that tests need to track. ";
        p += "surface field must be exactly \"" + s + "\". command field should be what you ran. ";
        p += "If the suite fully passes, passed=true and failures=[]. ";
        p += "Do not edit files in this phase. ";
        p += "Prefer path values that are repo-relative files or packages, e.g. internal/controlplane/store_services_test.go or console/src/lib/dashboard/core/service.server.test.ts. ";
    }

    let mode = "all";
    if s == "e2e-vm" {
        mode = "read-only";
    }
    discover_jobs.push(#{
        prompt: p,
        label: "discover:" + s,
        capability_mode: mode,
        output_schema: discovery_schema,
    });
}

let discover_results = parallel(discover_jobs);

// Collect failures; track which surfaces need verify later.
let failed_surfaces = [];
let raw_failures = [];
let discover_notes = [];
let di = 0;
for r in discover_results {
    let sname = "unknown";
    if di < active_surfaces.len() {
        sname = active_surfaces[di];
    }
    if r == () || !r.success || r.output == () {
        log("Discovery failed for surface " + sname);
        discover_notes.push(sname + ": discovery agent failed");
        // Treat as surface needing attention via a synthetic failure so we do not silently skip.
        raw_failures.push(#{
            surface: sname,
            id: sname + ":discovery-failed",
            path: "Makefile",
            pkg: "",
            test_name: "",
            title: "Discovery agent failed for " + sname,
            detail: "Re-run the surface manually; agent returned no structured output.",
            kind: "infra",
        });
        let already = false;
        for fs in failed_surfaces {
            if fs == sname { already = true; }
        }
        if !already {
            failed_surfaces.push(sname);
        }
    } else {
        let out = r.output;
        let passed = false;
        let sum = "";
        if type_of(out) == "map" {
            if out.passed != () && out.passed == true {
                passed = true;
            }
            if out.summary != () && type_of(out.summary) == "string" {
                sum = out.summary;
            }
        } else if type_of(out) == "string" {
            sum = out;
        }
        discover_notes.push(sname + ": passed=" + passed.to_string() + " — " + sum);
        if !passed {
            let already2 = false;
            for fs in failed_surfaces {
                if fs == sname { already2 = true; }
            }
            if !already2 {
                failed_surfaces.push(sname);
            }
        }
        if type_of(out) == "map" && out.failures != () && type_of(out.failures) == "array" {
            for f in out.failures {
                if type_of(f) != "map" {
                    // Canned or malformed entry — coerce to a fixable unit if string.
                    if type_of(f) == "string" && f != "" {
                        raw_failures.push(#{
                            surface: sname,
                            id: sname + ":raw",
                            path: "Makefile",
                            pkg: "",
                            test_name: "",
                            title: f,
                            detail: f,
                            kind: "test",
                        });
                    }
                } else {
                    let path = "";
                    if f.path != () && type_of(f.path) == "string" { path = f.path; }
                    let id = "";
                    if f.id != () && type_of(f.id) == "string" { id = f.id; }
                    let title = "";
                    if f.title != () && type_of(f.title) == "string" { title = f.title; }
                    let detail = "";
                    if f.detail != () && type_of(f.detail) == "string" { detail = f.detail; }
                    let kind = "test";
                    if f.kind != () && type_of(f.kind) == "string" { kind = f.kind; }
                    let pkg = "";
                    if f.pkg != () && type_of(f.pkg) == "string" { pkg = f.pkg; }
                    let tname = "";
                    if f.test_name != () && type_of(f.test_name) == "string" { tname = f.test_name; }
                    // Prefer path; fall back to pkg.
                    if path == "" && pkg != "" {
                        path = pkg;
                    }
                    if path == "" {
                        path = "Makefile";
                    }
                    if id == "" {
                        id = sname + ":" + path;
                    }
                    if title == "" {
                        title = "Failure in " + path;
                    }
                    raw_failures.push(#{
                        surface: sname,
                        id: id,
                        path: path,
                        pkg: pkg,
                        test_name: tname,
                        title: title,
                        detail: detail,
                        kind: kind,
                    });
                }
            }
        }
        // Harness paths as additional units if provided and surface failed.
        if !passed && type_of(out) == "map" && out.harness_paths != () && type_of(out.harness_paths) == "array" {
            for hp in out.harness_paths {
                if type_of(hp) == "string" && hp != "" {
                    raw_failures.push(#{
                        surface: sname,
                        id: sname + ":harness:" + hp,
                        path: hp,
                        pkg: "",
                        test_name: "",
                        title: "Harness may need update: " + hp,
                        detail: "Discovery listed this harness path for a failing surface.",
                        kind: "harness",
                    });
                }
            }
        }
    }
    di += 1;
}

log("Discovery complete: raw_failures=" + raw_failures.len().to_string() + " failed_surfaces=" + failed_surfaces.len().to_string());

// Early exit if everything green.
if raw_failures.len() == 0 && failed_surfaces.len() == 0 {
    let report = "";
    report += "# update-tests report\n\n";
    report += "Base: `" + base_ref + "`\n\n";
    report += "## Change summary\n\n" + change_summary + "\n\n";
    report += "## Discovery\n\nAll active surfaces passed. No test updates required.\n\n";
    for n in discover_notes {
        report += "- " + n + "\n";
    }
    let report_path = write_scratch_file("update-tests-report.md", report);
    complete(#{
        summary: "All active surfaces passed; no test updates needed",
        path: report_path,
        report: report,
        base_ref: base_ref,
        fixed: [],
        remaining: [],
        surfaces_verified: active_surfaces,
    });
}

// ============================================================================
// Shard into small non-overlapping units (file/package per surface)
// ============================================================================

// Skip pure infra failures for fix fan-out (nothing for a test author to update),
// but keep them in the report. Still allow compile/test/harness/assertion/missing.
let fixable = [];
let skipped_infra = [];
for f in raw_failures {
    let kind = "";
    if type_of(f) == "map" && f.kind != () && type_of(f.kind) == "string" {
        kind = f.kind;
    }
    if kind == "infra" {
        skipped_infra.push(f);
    } else {
        fixable.push(f);
    }
}

// Dedupe by surface|path; merge details.
let unit_keys = [];
let unit_map_surface = [];
let unit_map_path = [];
let unit_map_detail = [];
let unit_map_kinds = [];
let unit_map_ids = [];
let dropped_paths = 0;

for f in fixable {
    if type_of(f) != "map" {
        dropped_paths += 1;
    } else {
        let path = "";
        if f.path != () && type_of(f.path) == "string" { path = f.path; }
        if !path_allowed(path) {
            dropped_paths += 1;
            log("Dropped out-of-scope failure path: " + path);
        } else {
            let surf = "";
            if f.surface != () && type_of(f.surface) == "string" { surf = f.surface; }
            let k = unit_key(surf, path);
            let found = -1;
            let ui = 0;
            for uk in unit_keys {
                if uk == k {
                    found = ui;
                }
                ui += 1;
            }
            let title = "";
            if f.title != () && type_of(f.title) == "string" { title = f.title; }
            let tname = "";
            if f.test_name != () && type_of(f.test_name) == "string" { tname = f.test_name; }
            let kind = "test";
            if f.kind != () && type_of(f.kind) == "string" { kind = f.kind; }
            let detail = "";
            if f.detail != () && type_of(f.detail) == "string" { detail = f.detail; }
            let fid = "";
            if f.id != () && type_of(f.id) == "string" { fid = f.id; }

            let line = "";
            line += title;
            if tname != "" {
                line += " [" + tname + "]";
            }
            line += " (" + kind + "): " + detail;

            if found < 0 {
                unit_keys.push(k);
                unit_map_surface.push(surf);
                unit_map_path.push(path);
                unit_map_detail.push(line);
                unit_map_kinds.push(kind);
                unit_map_ids.push(fid);
            } else {
                // Append detail to existing unit.
                let prev = unit_map_detail[found];
                unit_map_detail[found] = prev + "\n---\n" + line;
            }
        }
    }
}

if dropped_paths > 0 {
    log("Dropped " + dropped_paths.to_string() + " failure path(s) outside allowlist");
}

// Cap units.
let unit_count = unit_keys.len();
let capped = false;
if unit_count > max_units {
    capped = true;
    log("Capping fix units from " + unit_count.to_string() + " to " + max_units.to_string());
    // Keep first max_units (stable order from discovery).
    let new_keys = [];
    let new_surf = [];
    let new_path = [];
    let new_detail = [];
    let new_kinds = [];
    let new_ids = [];
    let ci = 0;
    while ci < max_units {
        new_keys.push(unit_keys[ci]);
        new_surf.push(unit_map_surface[ci]);
        new_path.push(unit_map_path[ci]);
        new_detail.push(unit_map_detail[ci]);
        new_kinds.push(unit_map_kinds[ci]);
        new_ids.push(unit_map_ids[ci]);
        ci += 1;
    }
    unit_keys = new_keys;
    unit_map_surface = new_surf;
    unit_map_path = new_path;
    unit_map_detail = new_detail;
    unit_map_kinds = new_kinds;
    unit_map_ids = new_ids;
    unit_count = unit_keys.len();
}

if unit_count == 0 {
    let report2 = "";
    report2 += "# update-tests report\n\n";
    report2 += "Base: `" + base_ref + "`\n\n";
    report2 += "## Change summary\n\n" + change_summary + "\n\n";
    report2 += "## Discovery\n\nSurfaces reported issues but no fixable test units remained ";
    report2 += "(only infra failures, out-of-scope paths, or empty path lists).\n\n";
    for n in discover_notes {
        report2 += "- " + n + "\n";
    }
    report2 += "\n## Infra / skipped\n\n";
    for f in skipped_infra {
        let fs = "";
        let ft = "";
        let fd = "";
        if type_of(f) == "map" {
            if f.surface != () { fs = f.surface; }
            if f.title != () { ft = f.title; }
            if f.detail != () { fd = f.detail; }
        }
        report2 += "- [" + fs + "] " + ft + ": " + fd + "\n";
    }
    let report_path2 = write_scratch_file("update-tests-report.md", report2);
    complete(#{
        summary: "Failures observed but no fixable test units to edit",
        path: report_path2,
        report: report2,
        base_ref: base_ref,
        fixed: [],
        remaining: skipped_infra,
        surfaces_failed: failed_surfaces,
    });
}

// ============================================================================
// Phase 3: Parallel fix — one agent per small unit
// ============================================================================

phase("Fix");
log("Launching " + unit_count.to_string() + " parallel fix agent(s)");

// Precompute truncated diff once for all fix prompts.
let fix_diff = diff_for_prompt;

let fix_jobs = [];
let fi = 0;
while fi < unit_count {
    let usurface = unit_map_surface[fi];
    let upath = unit_map_path[fi];
    let udetail = unit_map_detail[fi];
    let uid = unit_keys[fi];
    let cmd = surface_command(usurface);

    let fp = "";
    fp += "You are updating OUTDATED TESTS / TEST HARNESSES after an intentional product code change. ";
    fp += "Assumption: tests passed before the change. Do NOT revert or rewrite product behavior to make old tests pass. ";
    fp += "Update tests and harnesses so they match the NEW intentional behavior.\n\n";
    fp += "Global change context (JSON): " + change_blob + "\n\n";
    fp += "Your owned unit (edit scope):\n";
    fp += "- unit_id: " + uid + "\n";
    fp += "- surface: " + usurface + "\n";
    fp += "- primary path: " + upath + "\n";
    fp += "- failure details:\n" + udetail + "\n\n";
    fp += "Rules:\n";
    fp += "1. Use tools: read the primary path, related harness helpers, and the production code under test. ";
    fp += "Use grep/read_file before editing. Do not answer from memory.\n";
    fp += "2. Edit ONLY this unit's test file(s) and tightly coupled harness helpers needed for this failure. ";
    fp += "Do not drive-by refactor unrelated tests. Do not edit production code unless it is a pure test-only helper mis-located ";
    fp += "and even then prefer test/harness files.\n";
    fp += "3. Prefer minimal, correct updates: assertions, fixtures, mocks, proto expectations, routes, env names, selectors.\n";
    fp += "4. After editing, re-run the narrowest useful command for THIS unit (package/file/test name). ";
    if usurface == "e2e-vm" {
        fp += "For e2e-vm: do NOT run make test-e2e-vm. Re-check statically that the harness matches new behavior.\n";
    } else {
        fp += "Surface full command for reference: `" + cmd + "`. Prefer a narrower go test / vitest / playwright filter when possible.\n";
    }
    fp += "5. status must be one of: fixed, partial, blocked, already_ok.\n";
    fp += "6. unit_id in the result MUST be exactly: " + uid + "\n";
    fp += "7. List files_changed with repo-relative paths you actually wrote.\n";
    fp += "8. If blocked (missing secrets, infra, ambiguous product intent), explain in notes and remaining — do not invent product behavior.\n\n";
    fp += "Optional diff excerpt vs " + base_ref + " (may be truncated; re-open files as needed):\n";
    fp += fix_diff;

    fix_jobs.push(#{
        prompt: fp,
        label: "fix:" + fi.to_string() + ":" + usurface,
        capability_mode: "all",
        output_schema: fix_schema,
    });
    fi += 1;
}

let fix_results = parallel(fix_jobs);

let fixed_units = [];
let partial_units = [];
let blocked_units = [];
let fix_i = 0;
for r in fix_results {
    let expect_id = unit_keys[fix_i];
    if r != () && r.success && r.output != () && type_of(r.output) == "map" {
        let st = "";
        if r.output.status != () && type_of(r.output.status) == "string" {
            st = r.output.status;
        }
        let notes = "";
        if r.output.notes != () && type_of(r.output.notes) == "string" {
            notes = r.output.notes;
        }
        let files = [];
        if r.output.files_changed != () && type_of(r.output.files_changed) == "array" {
            files = r.output.files_changed;
        }
        let entry = #{
            unit_id: expect_id,
            status: st,
            notes: notes,
            files_changed: files,
            surface: unit_map_surface[fix_i],
            path: unit_map_path[fix_i],
        };
        if st == "fixed" || st == "already_ok" {
            fixed_units.push(entry);
        } else if st == "partial" {
            partial_units.push(entry);
        } else {
            blocked_units.push(entry);
        }
    } else {
        blocked_units.push(#{
            unit_id: expect_id,
            status: "blocked",
            notes: "fix agent failed or returned no structured output",
            files_changed: [],
            surface: unit_map_surface[fix_i],
            path: unit_map_path[fix_i],
        });
    }
    fix_i += 1;
}

log("Fix phase: fixed=" + fixed_units.len().to_string()
    + " partial=" + partial_units.len().to_string()
    + " blocked=" + blocked_units.len().to_string());

// ============================================================================
// Phase 4: Verify — re-run surfaces that had failures (except e2e-vm: static re-check)
// ============================================================================

phase("Verify");

let verify_surfaces = [];
for s in failed_surfaces {
    // Always re-check surfaces that failed discovery.
    let exists = false;
    for v in verify_surfaces {
        if v == s { exists = true; }
    }
    if !exists {
        verify_surfaces.push(s);
    }
}

let verify_jobs = [];
for s in verify_surfaces {
    let cmd = surface_command(s);
    let vp = "";
    vp += "You are verifying whether surface \"" + s + "\" is healthy after parallel test updates. ";
    vp += "Global change context: " + change_blob + ". ";
    if s == "e2e-vm" {
        vp += "Do NOT run make test-e2e-vm. Statically re-check cmd/testvm/ and infra/test-vm/ against current product code. ";
        vp += "Use read_file/grep. Set passed=true only if you find no remaining outdated harness assumptions. ";
    } else {
        vp += "Run `" + cmd + "` from the repo root. Report remaining failures with names/paths. ";
        vp += "passed=true only if the command exits successfully / suite is green. ";
    }
    vp += "surface must be exactly \"" + s + "\". summary must mention what you ran or which files you re-read.";

    let vmode = "all";
    if s == "e2e-vm" {
        vmode = "read-only";
    }
    verify_jobs.push(#{
        prompt: vp,
        label: "verify:" + s,
        capability_mode: vmode,
        output_schema: verify_schema,
    });
}

let verify_results = [];
if verify_jobs.len() > 0 {
    verify_results = parallel(verify_jobs);
}

let surfaces_passed = [];
let surfaces_still_failing = [];
let verify_lines = [];
let vi = 0;
for r in verify_results {
    let sname = verify_surfaces[vi];
    if r != () && r.success && r.output != () && type_of(r.output) == "map" {
        let p = false;
        if r.output.passed != () && r.output.passed == true {
            p = true;
        }
        let sum = "";
        if r.output.summary != () && type_of(r.output.summary) == "string" {
            sum = r.output.summary;
        }
        if p {
            surfaces_passed.push(sname);
            verify_lines.push("- PASS " + sname + ": " + sum);
        } else {
            surfaces_still_failing.push(sname);
            let rem = "";
            if r.output.remaining_failures != () && type_of(r.output.remaining_failures) == "array" {
                for rf in r.output.remaining_failures {
                    if type_of(rf) == "string" {
                        rem += " | " + rf;
                    }
                }
            }
            verify_lines.push("- FAIL " + sname + ": " + sum + rem);
        }
    } else {
        surfaces_still_failing.push(sname);
        verify_lines.push("- FAIL " + sname + ": verify agent failed");
    }
    vi += 1;
}

// ============================================================================
// Phase 5: Report
// ============================================================================

phase("Report");

let report_md = "";
report_md += "# update-tests report\n\n";
report_md += "Base ref: `" + base_ref + "`\n\n";
report_md += "## Change summary\n\n";
report_md += change_summary + "\n\n";
if change_areas.len() > 0 {
    report_md += "### Changed areas\n\n";
    for a in change_areas {
        report_md += "- " + a + "\n";
    }
    report_md += "\n";
}
if api_changes.len() > 0 {
    report_md += "### API / behavior changes\n\n";
    for a in api_changes {
        report_md += "- " + a + "\n";
    }
    report_md += "\n";
}

report_md += "## Discovery\n\n";
for n in discover_notes {
    report_md += "- " + n + "\n";
}
report_md += "\nFixable units scheduled: " + unit_count.to_string();
if capped {
    report_md += " (capped from a larger set; raise args.max_units to cover more)";
}
report_md += "\n";
if skipped_infra.len() > 0 {
    report_md += "\n### Infra failures (not auto-fixed)\n\n";
    for f in skipped_infra {
        let fs = "";
        let ft = "";
        let fd = "";
        if type_of(f) == "map" {
            if f.surface != () { fs = f.surface; }
            if f.title != () { ft = f.title; }
            if f.detail != () { fd = f.detail; }
        }
        report_md += "- [" + fs + "] " + ft + " — " + fd + "\n";
    }
}

report_md += "\n## Fixes\n\n";
report_md += "Fixed/already_ok: " + fixed_units.len().to_string()
    + " | partial: " + partial_units.len().to_string()
    + " | blocked: " + blocked_units.len().to_string() + "\n\n";

if fixed_units.len() > 0 {
    report_md += "### Fixed\n\n";
    for u in fixed_units {
        report_md += "- `" + u.unit_id + "` (" + u.status + "): " + u.notes + "\n";
        if u.files_changed != () {
            for f in u.files_changed {
                report_md += "  - wrote: " + f + "\n";
            }
        }
    }
    report_md += "\n";
}
if partial_units.len() > 0 {
    report_md += "### Partial\n\n";
    for u in partial_units {
        report_md += "- `" + u.unit_id + "`: " + u.notes + "\n";
    }
    report_md += "\n";
}
if blocked_units.len() > 0 {
    report_md += "### Blocked\n\n";
    for u in blocked_units {
        report_md += "- `" + u.unit_id + "`: " + u.notes + "\n";
    }
    report_md += "\n";
}

report_md += "## Verification\n\n";
if verify_lines.len() == 0 {
    report_md += "No surfaces re-verified.\n";
} else {
    for line in verify_lines {
        report_md += line + "\n";
    }
}

report_md += "\n## Outcome\n\n";
if surfaces_still_failing.len() == 0 && blocked_units.len() == 0 && partial_units.len() == 0 {
    report_md += "All previously failing active surfaces are green after test updates.\n";
} else {
    report_md += "Some work remains. Still-failing surfaces: ";
    if surfaces_still_failing.len() == 0 {
        report_md += "(none)";
    } else {
        let first = true;
        for s in surfaces_still_failing {
            if !first {
                report_md += ", ";
            }
            report_md += s;
            first = false;
        }
    }
    report_md += ".\n";
    report_md += "Re-run: `/update-tests` with the same args, or raise `max_units` / fix blocked units manually.\n";
}

let final_path = write_scratch_file("update-tests-report.md", report_md);

let outcome = "updated tests";
if surfaces_still_failing.len() > 0 {
    outcome = "partial — some surfaces still failing";
} else if blocked_units.len() > 0 || partial_units.len() > 0 {
    outcome = "partial — some units blocked/partial but surfaces green or unchecked";
} else if fixed_units.len() == 0 && raw_failures.len() > 0 {
    outcome = "no fixes applied";
}

complete(#{
    summary: outcome + " (fixed=" + fixed_units.len().to_string()
        + " still_failing_surfaces=" + surfaces_still_failing.len().to_string() + ")",
    path: final_path,
    report: report_md,
    base_ref: base_ref,
    fixed_count: fixed_units.len(),
    partial_count: partial_units.len(),
    blocked_count: blocked_units.len(),
    surfaces_passed: surfaces_passed,
    surfaces_still_failing: surfaces_still_failing,
    units_scheduled: unit_count,
    capped: capped,
});

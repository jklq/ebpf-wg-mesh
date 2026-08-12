---
name: sweepingly-update-tests
description: >
  After product/code changes, run the repo's update-tests Grok workflow to refresh
  outdated tests and harnesses before you add new tests. Not for greenfield test
  authoring, do this before authoring your own tests. Use when **many** tests across the test pyramid likely broke from intentional changes, after a feature or refactor lands, or when asked to sync/update tests, /update-tests.
---

# Update tests (post-change)

Assumption: tests **passed before** the product change. That change is intentional — **update tests and harnesses** to match new behavior. Do **not** revert product code to make old tests pass.

This skill does **not** author new coverage. Run it **first** so the existing suite matches the change; write additional tests only **after** it finishes (or clearly has nothing left to fix).

## Surfaces

Workflow definition: `.grok/workflows/update-tests.rhai`

| Surface                                                               | How outdated tests are found                   |
| --------------------------------------------------------------------- | ---------------------------------------------- |
| unit-go, unit-console, integration-go, integration-console, e2e-local | **Run** the suite                              |
| e2e-vm                                                                | **Static analysis only** (never provision VMs) |

## Invoke via CLI (any agent)

Requires `grok` on `PATH`. There is no `grok workflow …` subcommand — headless Grok must call the **workflow tool** (or you use `/workflow` in the TUI).

`-p` / `--single` **takes the prompt as its value**. Put flags before `-p`, or after the prompt string — never `grok -p --yolo '…'` (that makes `--yolo` the prompt and errors).

`--yolo` is an alias for `--always-approve` (auto-approve tool runs). Prefer `--cwd` set to the repo root so project workflow `.grok/workflows/update-tests.rhai` is discovered.

With explicit args (preferred):

```bash
# base = git ref for the pre-change baseline (default in workflow: main)
# include_slow=false skips integration + e2e-local
# max_units caps parallel fix agents (default 20)

ROOT="$(git rev-parse --show-toplevel)"
ARGS='{"base":"main","include_slow":true,"max_units":20}'

grok --cwd "$ROOT" --verbatim --yolo -p \
  "Launch the project workflow named update-tests with args: ${ARGS}. Use the workflow tool (name=update-tests). Do not reimplement discovery/fix. Wait until the run completes, then print only the summary and report path."
```

Faster pass (units + static e2e-vm only):

```bash
ROOT="$(git rev-parse --show-toplevel)"
ARGS='{"base":"main","include_slow":false,"max_units":16,"surfaces":["unit-go","unit-console","e2e-vm"]}'

grok --cwd "$ROOT" --verbatim --yolo -p \
  "Launch the project workflow named update-tests with args: ${ARGS}. Use the workflow tool (name=update-tests). Wait until the run completes, then print the summary and report path."
```

Equivalent flag placement (prompt first, then flags) also works:

```bash
grok -p "Launch the project workflow named update-tests with args: ${ARGS}. Use the workflow tool." \
  --cwd "$ROOT" --verbatim --yolo
```

Inside an interactive Grok session:

```text
/workflow update-tests {"base":"main","include_slow":false,"max_units":16}
```

Watch live runs with `/workflows` (fullscreen TUI). Headless has no `/workflows` UI — the process should block until the agent finishes summarizing the run.

## Args (workflow)

| Field          | Default       | Meaning                                                                                           |
| -------------- | ------------- | ------------------------------------------------------------------------------------------------- |
| `base`         | `main`        | Pre-change ref for `git_diff_since`                                                               |
| `surfaces`     | all six       | Subset: `unit-go`, `unit-console`, `integration-go`, `integration-console`, `e2e-local`, `e2e-vm` |
| `max_units`    | `20` (max 48) | Parallel fix fan-out                                                                              |
| `include_slow` | `true`        | `false` drops integration + e2e-local                                                             |

Pick `base` as the last known-green commit/branch when it is not `main`.

## After the run

1. Read the workflow report (scratch path in the result, typically `update-tests-report.md`).
2. If surfaces still fail, re-run with a higher `max_units` or a narrowed `surfaces` list — or fix remaining units manually.
3. **Only then** add new tests for gaps the existing suite never covered.

## Do not

- Use this instead of writing tests for brand-new behavior that had no prior coverage.
- Revert product changes to green the suite.
- Run `make test-e2e-vm` as part of this skill (static analysis only).

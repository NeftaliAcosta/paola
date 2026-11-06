# PAOLA

P.A.O.L.A. (Polymorphic Autonomous Orchestrator for Local Agents) is a Go CLI that coordinates local AI coding tools through a persistent, review-driven roadmap.

PAOLA runs agents inside your project, optionally runs deterministic tests, requests independent review, and records each iteration locally. It supports `agy`, Codex, and Claude CLIs.

## Features

- Interactive wizard for context, objectives, roadmaps, rules, agents, models, tests, and retries.
- Coder → tests → Reviewer → Judge workflow with retries and escalation.
- Live split-screen dashboard with scrolling logs.
- Pause, skip, safe quit, reconfiguration, and headless execution.
- Git task-branch isolation, diff capture, and Conventional Commit-style messages.
- Encrypted and compressed manifests, agent logs, and diffs.
- Interactive history with timelines and file-by-file diffs.
- Role budgets, bounded context, incremental summaries, effort settings, and optional simulation.
- Per-role input/output token budgets with UTF-8-safe truncation and fallback estimation.
- Retryable failure detection with bounded backoff, persisted failure details, and resumable state.
- Optional per-execution Git worktrees (`--worktree` or `worktree_per_run: true`).

## Requirements

- Go 1.22 or newer.
- Git installed and initialized in the target project.
- At least one authenticated local agent CLI: `agy`, `codex`, or `claude`.

PAOLA does not initialize Git automatically. Repository creation and branch policy remain explicit developer decisions.

## Installation

```sh
go install github.com/neftaliacosta/paola/cmd/paola@latest
export PATH="$PATH:$(go env GOPATH)/bin"
paola --version
```

## Quick start

From the root of a Git project:

```sh
paola init
paola config
paola run
```

The wizard asks for a task title, project context, objective, roadmap, role instructions, agent CLI for each role, optional model names, test command, and retry limit.

For long text, use file-backed inputs:

```sh
paola init \
  --context-file docs/context.md \
  --objective-file docs/objective.md \
  --roadmap-file docs/roadmap.md \
  --rules-file docs/rules.md
```

## Commands

```text
paola init       Create or reconfigure the .paola workspace
paola config     Show configuration, agent health, and validation status
paola run        Run pending roadmap tasks with the live terminal UI
paola history    Browse tasks, iterations, timelines, and file diffs
paola update     Show self-update status
paola --version  Show version, platform, and Go runtime
```

Headless mode uses the current branch as the base unless one is supplied:

```sh
paola run --headless
paola run --headless --base-branch main
paola run --worktree

# Validate the workflow without invoking an agent or changing project files
paola run --simulate
```

Interactive mode asks for the base branch, then creates or checks out a task branch such as `paola/crud-de-usuarios`. The branch is never merged automatically. With `--worktree`, PAOLA creates the task branch directly from the selected base and runs in a sibling worktree without changing the original checkout.

## Controls

During a run:

```text
[p] pause/resume   [s] skip task   [q] safe quit
```

In history:

```text
[v] view files     [q/Esc] back     arrows/PageUp/PageDown scroll
```

## Workspace and history

`paola init` creates `.paola/` with configuration, prompts, roadmap, role instructions, active state, and encrypted history.

History manifests, agent logs, and task diffs are compressed and encrypted. By default, PAOLA creates `.paola/history/.key`. To use a shared deterministic key instead:

```sh
export PAOLA_SECRET_KEY='use-a-secret-from-your-secret-manager'
```

When run inside an existing Git repository, `paola init` adds `.paola/` to `.git/info/exclude`. This keeps prompts, logs, keys, and diffs out of `git status` without modifying the project's shared `.gitignore`.

## Agent configuration

Each role can use a different CLI and model:

```yaml
agents:
  coder: agy
  reviewer: codex
  judge: claude
models:
  coder: ""
  reviewer: ""
  judge: ""
```

Reconfigure with `paola init`; existing history is preserved.

### Cost and reliability controls

The wizard writes conservative defaults: one retry, a 30-minute no-output timeout, and bounded turns/context per role. Adjust them in `.paola/config.yaml` when needed:

```yaml
max_retries: 1
agent_idle_timeout_minutes: 30
reuse_sessions: false
budgets:
  coder:
    max_turns: 2
    effort: medium
    max_input_chars: 24000
    max_output_chars: 60000
    max_input_tokens: 6000
    max_output_tokens: 15000
  reviewer:
    max_turns: 2
    effort: low
    max_input_chars: 50000
    max_output_chars: 30000
    max_input_tokens: 12500
    max_output_tokens: 7500
  judge:
    max_turns: 1
    effort: low
    max_input_chars: 50000
    max_output_chars: 20000
    max_input_tokens: 12500
    max_output_tokens: 5000
retry_backoff_seconds: 3
worktree_per_run: false
```

PAOLA sends only the context relevant to each role and maintains a compact `.paola/state/context_summary.md`. Reviewer and Judge receive the task evidence and diff, not the full project context. History records per-role CLI, model, effort, turns, input/output size, duration, and status.

`reuse_sessions` is opt-in and currently best-effort: when a compatible CLI emits a session identifier, PAOLA can resume it on later turns. Claude uses `--resume` by default. Other CLIs can define their own adapter in `commands.<cli>.session_args`, for example `--session {{session_id}}`. If no identifier is emitted, it safely starts a fresh invocation.

When an agent reports a rate limit, quota exhaustion, timeout, or temporary capacity error, PAOLA retries only within `max_retries` and waits `retry_backoff_seconds` between attempts. Other failures stop the roadmap gate and are saved in the encrypted run report. `paola run --simulate` exercises the orchestration flow without invoking agents or changing project files.

## Git behavior

Before orchestration, `paola run` lets you choose a base branch and creates or checks out an isolated task branch. It captures diffs excluding `.paola/`, creates semantic commits for completed tasks, and leaves the task branch checked out. PAOLA never merges automatically.

## Troubleshooting

If an execution fails, choose `View failure details` before retrying. The report shows the failed task, error, attempts, review rounds, tokens, files, and available diff.

```sh
git status
git branch --show-current
command -v agy
command -v codex
command -v claude
paola config
```

## Development

```sh
gofmt -w cmd internal
go vet ./...
go test -race ./...
go build -o paola ./cmd/paola
```

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

PAOLA is distributed under the [MIT License](LICENSE).

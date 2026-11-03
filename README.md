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
```

Interactive mode asks for the base branch, then creates or checks out a task branch such as `paola/crud-de-usuarios`. The branch is never merged automatically.

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

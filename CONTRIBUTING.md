# Contributing to PAOLA

Keep changes focused, portable across macOS, Linux, and Windows, and explain behavior changes in the pull request.

## Local workflow

```sh
gofmt -w cmd internal
go vet ./...
go test -race ./...
go build -o paola ./cmd/paola
```

Before opening a pull request:

- Add or update tests for behavior changes.
- Do not commit `.paola/`, `.codegraph/`, snapshots, binaries, or local logs.
- Preserve existing configuration and history formats when possible.
- Avoid OS-specific credential managers or external binaries unless required.
- Document new commands, flags, and keyboard shortcuts in the README.

## Pull requests

Describe the user-facing behavior, implementation approach, validation performed, and compatibility concerns. Keep unrelated refactors out of the same pull request.

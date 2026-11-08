# PAOLA orchestration audit remediation

## Scope

This change hardens durable execution history. It does not change project
worktrees, Springo, or previously stored history contents.

## Decision protocol

Reviewer output must contain exactly one non-quoted, non-fenced line:

- `STATUS: LGTM`
- `STATUS: CHANGES_REQUESTED`

Judge output must contain exactly one non-quoted, non-fenced line:

- `VERDICT: OVERRIDE_APPROVE`
- `VERDICT: FINAL_INSTRUCTIONS`

Narrative approvals, bullets, examples, contradictory markers, and bare
`APPROVE`/`PASS` are rejected. The original encrypted transcript remains
available through its event `output_ref`; the event stores the normalized
decision.

## Attempt evidence

Transcripts remain encrypted and are stored per task/attempt/role. Diffs are
stored as `diffs/task-NNN-attempt-MM.patch.enc`, while legacy readers retain
the old task-only path. Test evidence is stored as encrypted JSON under
`evidence/tasks/NNN/attempt-MM-tests.enc` and includes command, working
directory, timestamps, duration, exit code, status, and output. The mutable
`state/test_output.log` is only a live compatibility artifact.

## Legacy history

Old manifests are readable. When a legacy manifest lacks an event ledger,
PAOLA synthesizes migration events with `synthetic: true`; these records are
explicitly historical reconstruction, not direct agent evidence.

## Export

Export an execution explicitly:

```sh
paola history export <execution-id> --output ./paola-audit-export
```

The export decrypts selected audit artifacts, redacts common credential
patterns, and writes `SHA256SUMS`. Source encrypted history is never modified.

## Validation

Required checks for this remediation:

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o paola ./cmd/paola
```

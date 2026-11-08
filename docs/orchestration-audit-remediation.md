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

The default export contains only `manifest.json`, `events.json`,
`integrity.json`, and `SHA256SUMS`. It validates every referenced encrypted
artifact but does not decrypt it into the bundle. Use
`--include-sensitive` only when needed; it exports redacted decrypted
transcripts, diffs, and test evidence and prints a warning. The destination
must be new, is created with mode `0700`, and files use `0600`. Traversal,
absolute paths, symlinks, corrupt/missing references, and partial checksum
generation are rejected. Source encrypted history is never modified.

## Acceptance evidence

| Criterion | Implementation evidence |
| --- | --- |
| Unique execution/task/attempt identity | `Manifest.ID`, event sequence/ID, task index, and attempt-scoped transcript/diff paths |
| Chronological agent decisions | Append-only persisted `Event` ledger with sequence and UTC timestamp |
| Reviewer/Judge protocol | Exactly one strict marker; fenced, quoted, bulleted, duplicate, and narrative markers rejected |
| Test evidence | Encrypted per-attempt JSON reference; `tests_started` has no pre-reference; retries reference persisted evidence |
| Bonus Coder trace | `agent_started`, `bonus_coder_finished`, test start/finish, final diff, and task finish events |
| Failure/retry reconstruction | Error, status, attempt, retry event, output reference, and test evidence are persisted |
| Legacy compatibility | Legacy manifests are synthesized with `synthetic: true`; missing synthetic transcripts are not required |
| Secure export | Safe metadata-only default, optional redacted sensitive mode, path/symlink validation, atomic failure cleanup, checksums |

## Verdict

`REMEDIATION_PASS`

Release-candidate validation passed:

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `go vet ./...`
- `go build -o /tmp/paola-audit-build ./cmd/paola`
- `git diff --check`

The implementation does not claim that historical synthetic events are
equivalent to original live agent transcripts; that limitation remains
explicit in the exported metadata.

## Validation

Required checks for this remediation:

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o paola ./cmd/paola
```

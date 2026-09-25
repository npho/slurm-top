# Contributor guide for AI agents

## Project overview

This repository is a small Go command-line application named `slurm-top`. It
queries Slurm with `scontrol show node -o` and `squeue --json`, then renders
node and per-user allocation views. It is a single `package main` module;
there is no server, database, or generated-code step.

Start by reading `README.md`, `go.mod`, the relevant implementation file, and
its corresponding `*_test.go` file. Keep the distinction between **Slurm
allocations** and observed hardware utilization explicit: this program reports
allocations and pending requests, not sampled CPU/GPU/RSS usage.

## Local workflow

Use the Makefile targets before submitting a change:

```sh
make fmt
make test
make vet
make build
```

Equivalent commands are `go fmt ./...`, `go test ./...`, `go vet ./...`, and
`go build -o slurm-top .`. `make clean` removes the locally built binary.

Tests should not require a live Slurm controller. Prefer testing parsers and
renderers with fixture strings or JSON, as the existing tests do. Do not run
commands that mutate Slurm state.

## Go practices

- Target the Go version declared in `go.mod`; do not lower it without an
  explicit compatibility decision.
- Run `gofmt` (or `make fmt`) on every changed Go file. Use idiomatic Go and
  the standard library before adding dependencies.
- Keep functions small and place behavior with its related renderer or
  collector. Avoid unrelated refactors in a focused change.
- Propagate errors with context (`fmt.Errorf("operation: %w", err)`) and do not
  silently discard meaningful errors.
- Put time-bounded external work behind `context.Context`. The CLI's Slurm
  commands must honor `--timeout` and Ctrl-C.
- Preserve safe terminal behavior: restore terminal state/alternate screen and
  mouse mode on every exit path, and keep non-terminal output pipe-friendly.
- Sanitize any scheduler-provided text before rendering it to a terminal; avoid
  introducing terminal-control-sequence injection.
- Preserve output compatibility deliberately. JSON field names are part of the
  machine-readable interface; use stable structs and add tests for changes.
- Add or update focused table-driven tests for parsing, sorting, formatting,
  flags, or regressions. Test error cases as well as the success path.
- Run `go mod tidy` only when dependencies actually change, and review both
  `go.mod` and `go.sum`. Do not hand-edit indirect dependency versions.

## Slurm domain rules

- Use `tres_alloc_str` only for running-job allocations.
- Keep `tres_req_str` pending demand separate; never substitute requested
  resources for running allocations.
- Treat capacity from `scontrol` as scheduler capacity, not telemetry.
- Generic GPU TRES must not be mislabeled as H200 or MIG resources.
- Node-wide `FreeMem` must not be represented as per-user memory use.

## Git practices

- Work on a descriptive branch when a Git repository is available; keep each
  commit focused on one logical change.
- Inspect `git status` and `git diff` before committing. Stage only intended
  files and do not commit build products such as the `slurm-top` binary, test
  caches, editor files, or credentials.
- Write imperative, concise commit subjects (for example,
  `Add JSON snapshot validation`). Explain non-obvious rationale in the commit
  body.
- Rebase or merge according to the repository's established workflow; do not
  rewrite shared history without explicit approval.
- Do not alter unrelated formatting, generated files, dependencies, or public
  output while implementing a focused fix.

## Documentation and review checklist

Update `README.md` when changing flags, output formats, requirements,
interactive controls, or metric semantics. Before handing off work, report:

1. files changed and why;
2. validation commands run and their results; and
3. any limitation that could not be tested locally (especially live Slurm
   behavior).

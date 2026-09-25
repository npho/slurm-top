# slurm-top

`slurm-top` is a terminal dashboard and snapshot tool for viewing Slurm cluster
allocations. It combines node capacity from `scontrol` with queued and running
job allocations from `squeue`, then presents cluster and per-user CPU, GPU, and
memory totals.

> Allocation is not utilization. The reported figures are Slurm resource
> allocations; they are not sampled CPU use, GPU use, or process RSS.

## Requirements

- Go 1.23 or later (to build from source)
- A Slurm environment with `scontrol` and `squeue` on `PATH`
- Permission to query the Slurm controller

## Build and test

```sh
make build             # builds ./slurm-top
make test
make vet
make fmt
```

Or build directly with `go build -o slurm-top .`. Remove the local binary with
`make clean`.

## Usage

```sh
# Opens the live terminal UI when stdout is a terminal.
./slurm-top

# Print a one-time, pipe-friendly snapshot.
./slurm-top | less

# Select the initial user sort (gpu is the default).
./slurm-top --sort mem

# Emit one JSON snapshot; with --watch, output is newline-delimited JSON.
./slurm-top --format json
./slurm-top --format json --watch 5s > snapshots.ndjson

# Refresh noninteractive output every five seconds.
./slurm-top --format top --watch 5s

# Use the node-oriented views.
./slurm-top --format grid
./slurm-top --format detail --all
```

Run `./slurm-top --help` for all flags. Key options are:

| Option | Description |
| --- | --- |
| `--format top|json|grid|detail|table` | Select the output format (`top` is the default). |
| `--sort gpu|cpu|mem|jobs|user` | Initial sort for the per-user view. |
| `--watch 5s` | Refresh interval; zero (the default) prints once outside the interactive UI. |
| `--timeout 10s` | Timeout for each Slurm query. |
| `--interactive` | Require/use an alternate-screen terminal UI. |
| `--all` | Include non-GPU nodes in node-oriented views. |

`--format dashboard` remains accepted as a compatibility alias for `grid`.

## Interactive controls

The default `top` UI refreshes every five seconds when attached to a terminal.
Use **Up/Down** or the mouse wheel to select a user, **Right** or **Enter** to
open that user's job list, and **Left** to return. Click a column heading to
sort (click again to reverse it), or click a row to select it. Keyboard sorting
shortcuts are `g` (GPU), `c` (CPU), `m` (memory), `j` (running jobs), and `u`
(username); `r` refreshes; `q` or Ctrl-C exits.

Mouse support requires an SGR-mouse-compatible terminal. The program restores
the screen and mouse mode on exit. Set `NO_COLOR=1` or `CLICOLOR=0` to disable
color in supported node displays.

## Data and metric semantics

- Running-job totals come from `squeue --json` `tres_alloc_str`.
- Pending demand comes from `tres_req_str` and is shown separately from live
  allocations.
- Capacity comes from `scontrol show node -o`.
- H200 and H200 MIG bars classify typed GPU TRES. Other or generic GPU TRES are
  not included in those H200-specific bars.
- A failed refresh retains the prior interactive snapshot and reports the
  error.

Slurm capacity, reserved capacity, and allocated resources may not agree
exactly. `FreeMem` is node-wide and cannot be attributed to a user; this tool
deliberately does not infer per-user observed usage. A telemetry or accounting
backend with per-job access would be required for measured utilization.

## Project layout

- `main.go` — CLI parsing, node collection, and node-oriented rendering
- `top.go`, `top_run.go`, `top_interactive.go` — per-user snapshot collection
  and terminal UI
- `grid.go`, `dashboard.go`, `interactive.go` — node-oriented display helpers
- `*_test.go` — parser, rendering, and UI behavior tests
- `Makefile` — standard build, test, vet, format, and clean targets

See [AGENTS.md](AGENTS.md) for contribution guidance.

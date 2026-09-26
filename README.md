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

## Install release packages

GitHub releases include Linux `amd64` packages:

```sh
# Rocky Linux / RHEL-family systems
sudo dnf install ./slurm-top_*.x86_64.rpm

# Ubuntu / Debian-family systems
sudo apt install ./slurm-top_*_amd64.deb
```

The package installs `slurm-top` to `/usr/bin/slurm-top`, which is the
appropriate path for distribution-managed executables. Use `/usr/local/bin`
for a manual, administrator-local installation that is not managed by a
package. Slurm client commands (`squeue` and `scontrol`) and controller-query
permission are still required at runtime.

Packages also install the manual page. After installation, run:

```sh
man slurm-top
```

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
The summary defaults to running GPU allocation descending; opening a user
shows that user's jobs sorted by elapsed time descending. Use **Up/Down** or
the mouse wheel to select a user, **Right** or **Enter** to open their job
list, and **Left** to return. Returning restores the prior summary sort and
selection.

Press **Up** from the first row to focus the column header; use **Left/Right**
to choose a column and **Enter** to sort it descending, then ascending on the
next Enter. Click a column heading to sort (click again to reverse it), or
click a row to select it. In a job table that is wider than the terminal, use
**Left/Right** while a row is focused to scroll horizontally; **Left** at its
left edge returns to the summary. Keyboard sorting shortcuts are `g` (GPU),
`c` (CPU), `m` (memory), `j` (running jobs), and `u` (username); `r` refreshes;
`q` or Ctrl-C exits.

Summary columns show running and pending `GPU`, `CPU`, `C:G` (CPU per GPU),
`MEM`, and `M:C` (decimal GB per CPU). Job columns include account, QoS,
progress, elapsed/requested time, partition, resources, and job name. Columns
size themselves to visible content and reserve sort-arrow space.

Mouse support requires an SGR-mouse-compatible terminal. The program restores
the screen and mouse mode on exit. Set `NO_COLOR=1` or `CLICOLOR=0` to disable
color in supported node displays.

## Data and metric semantics

- Running-job totals come from `squeue --json` `tres_alloc_str`.
- Pending demand comes from `tres_req_str` and is shown separately from live
  allocations.
- Capacity comes from `scontrol show node -o`.
- The interactive header uses separate CPU, memory, and GPU boxes. Allocation
  bars use capacity on allocatable nodes only; the non-bold line beneath each
  bar reports unschedulable capacity as a share of all scheduler capacity. GPU
  shows H200 and H200-MIG allocations separately; generic or other GPU types
  remain part of aggregate GPU totals but not those typed bars.
- Job QoS, partition, start/end time, and requested time limit come from
  `squeue --json`. Elapsed time is rendered as
  `percent% [elapsed|requested]`; unknown or unlimited requested time displays
  `-`.
- Memory values in tables are rounded decimal GB; aggregate memory summaries
  use decimal TB.
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

## Publishing a release

GitHub Actions packages a published GitHub release using
[GoReleaser](https://goreleaser.com/). To publish, push the intended version
commit, create an annotated tag such as `v1.0.0`, push the tag, then use
GitHub's **Releases → Draft a new release** flow and publish that tag. The
workflow runs tests and vet, builds the Linux `amd64` binary, creates `.deb`
and `.rpm` packages (including the manual page), and uploads them to the
release.

See [AGENTS.md](AGENTS.md) for contribution guidance.

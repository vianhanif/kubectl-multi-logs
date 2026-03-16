# kubectl-multi-logs

Tail logs from multiple Kubernetes pods simultaneously — real-time or historical — with a Homebrew-style live progress display.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

## Features

- **Parallel execution**: Pod lookups, container fetches, and log streams run concurrently via goroutines.
- **Replica coverage**: Automatically includes all replicas of each app.
- **Real-time & historical modes**: Follow logs live or retrieve from a relative time (`10m`, `1h`, `2d`).
- **Flexible filtering**: Case-insensitive pattern matching with `|` OR support; dedicated `-e` flag for errors.
- **Organised output**: Lines prefixed with `[pod:container]` for easy grepping.
- **Brew-style live progress**: Scroll-and-advance terminal UI — each item prints a permanent ✔ line as it completes, grouped by app, with a live spinner for remaining streams.
- **Fixed semantic coloring**: Pod names are always cyan; container names are plain — consistent and easy to scan without distracting per-app palette shifts.
- **Exit summary with timestamps**: On completion a per-container summary table shows pod, container, line count, and `HH:MM:SS → HH:MM:SS` first/last log timestamps.
- **Debug command log**: CLI output (start banner through summary) is always mirrored to `command.log` in the same directory as the binary. The file is overwritten on each run.
- **Single binary**: No shell runtime required — just `kubectl` on `$PATH`.

## Installation

**Prerequisites**: Go 1.21+ and `kubectl` configured on `$PATH`.

```bash
git clone https://github.com/vianhanif/kubectl-multi-logs.git
cd kubectl-multi-logs
go build -o kubectl-multi-logs .
```

To install as a `kubectl` plugin (enables `kubectl multi-logs`):

```bash
cp kubectl-multi-logs /usr/local/bin/kubectl-multi-logs
```

### Shell alias

```bash
mylogs () {
    ~/path/to/kubectl-multi-logs "$@" app-1 app-2 app-3
}
```

## Usage

```
kubectl-multi-logs [-n namespace] [-s since] [-T timeout] [-g pattern] [-e] [-o [output_file]] <app1> <app2> ...
```

### Options

| Flag | Description | Default |
|---|---|---|
| `-n` | Kubernetes namespace | current context |
| `-s` | Historical logs since (e.g. `10m`, `1h`, `2d`) | follow live |
| `-T` | Per-stream timeout in collect mode (`-s`); `0` = no limit | `2m` |
| `-g` | Filter lines — case-insensitive, `\|` for OR | none |
| `-e` | Filter for `ERROR\|WARN\|Exception\|failed\|error` | off |
| `-o` | Output file name (`-o` alone uses default) | `tail_multiple_logs_data.log` |
| `-verbose` | Show per-pod/container rows during progress (default: compact 3-bar view) | off |

## Examples

To discover available app names in your cluster:

```bash
kubectl get pods -o jsonpath='{.items[*].metadata.labels.app}' | tr ' ' '\n' | sort | uniq
```

- Follow live logs from `agent-service` and `tez-api`:
  ```bash
  kubectl-multi-logs agent-service tez-api
  ```

- Last 30 minutes of logs, errors only:
  ```bash
  kubectl-multi-logs -s 30m -e agent-service
  ```

- Filter errors in `production` namespace:
  ```bash
  kubectl-multi-logs -n production -e tez-api
  ```

- Search for a pattern across apps and save to custom file:
  ```bash
  kubectl-multi-logs -g ERROR -o custom_logs.log agent-service tez-api
  ```

- Use `-o` alone to write to the default file:
  ```bash
  kubectl-multi-logs -s 15m -o agent-service tez-api
  ```

## Example Output

### Terminal Output

The tool runs through three phases with live-updating progress bars (go-pretty `StyleBlocks`, green `█` fill). By default all three phases are shown as compact single-line bars. Pass `-verbose` for per-item detail.

**Default — compact 3-bar view** (one bar per phase, all live at once):

Mid-run (phase 2 in progress):
```
Collecting from last 10m  ·  saving to /path/to/tail_multiple_logs_data.log
  Finding pods... (3/3)                    ...  [  1.342s]
  Fetching containers... (16/21)           ...  ║██████████████████▒░░░░░░░░░░░░░░░░░║  [  2.1s]
  Collecting logs...                       ...  ║░░░░░░░░░░░░░░░░▒█▒░░░░░░░░░░░░░░░░║  [  2.1s]
```

Completed:
```
Collecting from last 10m  ·  saving to /path/to/tail_multiple_logs_data.log
  Finding pods... (3/3)                    ...  [  1.342s]
  Fetching containers... (21/21)           ...  [  2.801s]
  Collecting logs... (23/23)               ...  [ 10.450s]
✔ All streams finished  total elapsed: 15s
```

Bars are **green** (`█`); they refresh live then become static lines once each phase finishes. The indeterminate spinner (`▒█▒`) is shown while the total is not yet known.

**With `-verbose` — per-item rows appear as results stream in:**

Phase 1 — completed (overall bar replaced by static line, items below it):
```
  Finding pods for apps...  3 / 3           ...  [  1.342s]
  agent-service                                            15 pod(s)  ...  [  312ms]
  agent-service-worker                                      3 pod(s)  ...  [  456ms]
  tez-api                                                   3 pod(s)  ...  [  623ms]
```

Phase 2 — completed:
```
  Fetching containers for pods...  21 / 21  ...  [  2.801s]
  agent-service-deployment-7b54bf66c7-2f79w      1 container(s)  ...  [  92ms]
  agent-service-deployment-7b54bf66c7-425mh      1 container(s)  ...  [ 108ms]
  agent-service-worker-deployment-6b5b6cbdf7-bbkll  2 container(s)  ...  [ 134ms]
  tez-api-deployment-77d5fb6445-hknxx            1 container(s)  ...  [ 187ms]
  ...
```

Phase 3 — collecting logs, grouped by app:
```
  Collecting logs… (21 / 21 done)          ...  [ 10.450s]
  agent-service
    [agent-service-deployment-7b54bf66c7-2f79w] agent-service   collected · 1557 lines  ...  [  9.2s]
    [agent-service-deployment-7b54bf66c7-425mh] agent-service   collected · 1551 lines  ...  [  9.8s]
  agent-service-worker
    [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-cron-worker   collected · 153 lines  ...  [  8.1s]
    [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-service-worker  collected · 61 lines  ...  [  7.5s]
  tez-api
    [tez-api-deployment-77d5fb6445-hknxx] tez-api               collected · 14504 lines  ...  [ 10.2s]
```

Pod names are displayed in **cyan**; container names are in plain text — fixed colours regardless of which apps are being tailed.

**Summary** (printed on completion or Ctrl+C):
```
── Summary

╭──────────────────────────────────────────────────────────────────┬────┬───────┬──────────────────────╮
│                                                                  │    │ Lines │ Time range           │
├──────────────────────────────────────────────────────────────────┼────┼───────┼──────────────────────┤
│ agent-service  2 pod(s) · 2 stream(s) · 3108 lines              │    │       │                      │
│   agent-service-deployment-7b54bf66c7-2f79w                      │    │       │                      │
│     agent-service                                                │ ✔  │  1557 │ 10:12:03 → 10:22:41  │
│   agent-service-deployment-7b54bf66c7-425mh                      │    │       │                      │
│     agent-service                                                │ ✔  │  1551 │ 10:12:04 → 10:22:39  │
├──────────────────────────────────────────────────────────────────┼────┼───────┼──────────────────────┤
│ agent-service-worker  1 pod(s) · 2 stream(s) · 214 lines        │    │       │                      │
│   agent-service-worker-deployment-6b5b6cbdf7-bbkll               │    │       │                      │
│     agent-cron-worker                                            │ ✔  │   153 │ 10:12:05 → 10:22:10  │
│     agent-service-worker                                         │ ✔  │    61 │ 10:12:05 → 10:20:44  │
├──────────────────────────────────────────────────────────────────┼────┼───────┼──────────────────────┤
│ tez-api  1 pod(s) · 1 stream(s) · 14504 lines                   │    │       │                      │
│   tez-api-deployment-77d5fb6445-hknxx                            │    │       │                      │
│     tez-api                                                      │ ✔  │ 14504 │ 10:12:01 → 10:22:58  │
├──────────────────────────────────────────────────────────────────┼────┼───────┼──────────────────────┤
│ TOTAL  3 app(s) · 4 pod(s) · 5 stream(s)                        │    │ 17826 │                      │
╰──────────────────────────────────────────────────────────────────┴────┴───────┴──────────────────────╯
```

The hierarchy is **app → pod → container**. Failed streams show `✗` with a truncated error message instead of line counts and timestamps.

### Log File Output

Each log line is prefixed with `[pod:container]` for easy identification:

```
[agent-service-5d4b8f6c9-abc12:agent-service] 2026-02-18 10:23:01 INFO  Starting request processing for policy #98712
[agent-service-5d4b8f6c9-abc12:agent-service] 2026-02-18 10:23:01 INFO  Fetching agent data for agent_id=4521
[agent-service-5d4b8f6c9-xyz34:agent-service] 2026-02-18 10:23:02 INFO  Starting request processing for policy #98713
[tez-api-6f8b9c7d4-xk2mn:tez-api] 2026-02-18 10:23:02 INFO  Received webhook callback for policy #98712
[tez-api-6f8b9c7d4-xk2mn:celery-worker] 2026-02-18 10:23:03 INFO  Processing task: send_notification policy_id=98712
[agent-service-5d4b8f6c9-abc12:agent-service] 2026-02-18 10:23:03 ERROR Failed to fetch provider response: timeout after 30s
[tez-api-6f8b9c7d4-pq5rs:tez-api] 2026-02-18 10:23:04 WARN  Retrying request to agent-service (attempt 2/3)
[agent-service-5d4b8f6c9-xyz34:nginx] 2026-02-18 10:23:04 10.0.0.1 - - "POST /api/v1/policies HTTP/1.1" 504 0
[tez-api-6f8b9c7d4-xk2mn:celery-worker] 2026-02-18 10:23:05 ERROR Task failed: send_notification ConnectionError
[tez-api-6f8b9c7d4-pq5rs:tez-api] 2026-02-18 10:23:05 INFO  Successfully retried request for policy #98712
```

> **Tip**: This output file can be shared with GitHub Copilot or other AI tools to analyze and correlate errors across pods.
> <img width="1680" height="1050" alt="Screenshot 2026-02-18 at 11 33 35 PM" src="https://github.com/user-attachments/assets/0b9088d6-9db3-48a8-bf89-8bf143832bb6" />

## Bash version

The original bash implementation is kept at [`versions/bash/tail_multiple_logs.sh`](versions/bash/tail_multiple_logs.sh) for reference. It has the same flags and behaviour but uses background subshells and temp files for concurrency.

## Code layout

| File | Responsibility |
|---|---|
| `config.go` | All tunable constants — timeouts, truncation widths, timestamp format, progress update frequency |
| `main.go` | Flag parsing, output-file setup, signal handler, top-level orchestration |
| `kubectl.go` | Domain types (`appPod`, `podContainers`) and `kubectl` helper functions |
| `phases.go` | Parallel pod/container discovery workers (`findPods`, `fetchContainers`) and the four phase runners (`runPhase1`, `runPhase2`, `runPhase1Clean`, `runPhase2Clean`) |
| `stream.go` | `streamState`, `streamConfig`, `streamLogs`, `matchesPattern`, `launchStreams` |
| `progress.go` | `newPW`, `phaseMonitor`, `displayMonitor`, `displayMonitorClean`, `cleanLabel`, `activeStop` |
| `summary.go` | `printSummary` — rounded table grouped by app → pod → container using go-pretty/table |

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

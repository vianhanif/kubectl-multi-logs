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
kubectl-multi-logs [-n namespace] [-s since] [-g pattern] [-e] [-o [output_file]] <app1> <app2> ...
```

### Options

| Flag | Description | Default |
|---|---|---|
| `-n` | Kubernetes namespace | current context |
| `-s` | Historical logs since (e.g. `10m`, `1h`, `2d`) | follow live |
| `-g` | Filter lines — case-insensitive, `\|` for OR | none |
| `-e` | Filter for `ERROR\|WARN\|Exception\|failed\|error` | off |
| `-o` | Output file name (`-o` alone uses default) | `tail_multiple_logs_data.log` |

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

The script runs through three phases, each showing a live brew-style progress display:

**Phase 1 — Finding pods** (one permanent line per app as results arrive):
```
Finding pods for apps...
  ✔  agent-service                                      15 pod(s)
  ✔  agent-service-worker                                3 pod(s)
  ✔  tez-api                                             3 pod(s)
  ⠸  Waiting for 2 more...
```

**Phase 2 — Fetching containers** (one permanent line per pod):
```
Fetching containers for pods...
  ✔  agent-service-deployment-7b54bf66c7-2f79w           1 container(s)
  ✔  agent-service-worker-deployment-6b5b6cbdf7-bbkll    2 container(s)
  ✔  tez-api-deployment-77d5fb6445-hknxx                 1 container(s)
  ⠼  Waiting for 4 more...
```

**Phase 3 — Collecting / following logs** (grouped by app, spinner shows overall progress):
```
Showing historical logs from 10m to now for 21 pods and their containers.
Logs are being saved to: /path/to/tail_multiple_logs_data.log
Press Ctrl+C to stop all log streams
----------------------------------------
  agent-service
    ✔ [agent-service-deployment-7b54bf66c7-2f79w] agent-service      Collected  1557 lines
    ✔ [agent-service-deployment-7b54bf66c7-425mh] agent-service      Collected  1551 lines
  agent-service-worker
    ✔ [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-service-worker   Collected    61 lines
    ✔ [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-cron-worker      Collected   153 lines
  tez-api
    ✔ [tez-api-deployment-77d5fb6445-hknxx] tez-api                  Collected 14504 lines
  ⠸  Collecting logs... (5/21 streams done)

Historical logs collection completed. Logs saved to: /path/to/tail_multiple_logs_data.log
```

Pod names are displayed in **cyan**; container names are in plain text — fixed colors regardless of which apps are being tailed.

**Summary** (printed on completion or Ctrl+C):
```
── Summary

  agent-service  2 pod(s) · 2 stream(s) · 3108 lines
    ✔ [agent-service-deployment-7b54bf66c7-2f79w] agent-service            1557 lines  10:12:03 → 10:22:41
    ✔ [agent-service-deployment-7b54bf66c7-425mh] agent-service            1551 lines  10:12:04 → 10:22:39

  agent-service-worker  1 pod(s) · 2 stream(s) · 214 lines
    ✔ [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-cron-worker   153 lines  10:12:05 → 10:22:10
    ✔ [agent-service-worker-deployment-6b5b6cbdf7-bbkll] agent-service-worker  61 lines  10:12:05 → 10:20:44

  tez-api  1 pod(s) · 1 stream(s) · 14504 lines
    ✔ [tez-api-deployment-77d5fb6445-hknxx] tez-api                        14504 lines  10:12:01 → 10:22:58

  ────────────────────────────────────────────────────────────────────────
  TOTAL   4 pod(s)   5 stream(s)   17826 lines
```

Failed streams show `✗` with a truncated error message instead of line counts and timestamps.

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

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

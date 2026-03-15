# kubectl-multi-logs (Go)

A Go rewrite of `tail_multiple_logs.sh` — tail logs from multiple Kubernetes pods simultaneously, with a Homebrew-style live progress display.

## Why Go?

| | Bash | Go |
|---|---|---|
| Concurrency | Background subshells + temp files | Goroutines + channels |
| Progress display | Polling temp files every 100 ms | Atomic counters, no disk I/O |
| Distribution | Requires bash + kubectl on PATH | Single static binary |
| Plugin ecosystem | Shell script | Native `kubectl-*` plugin naming |
| Error handling | Exit codes + manual checks | Typed errors |

## Prerequisites

- Go 1.21 or later
- `kubectl` configured and accessible on `$PATH`

## Build

```bash
cd go/
go build -o kubectl-multi-logs .
```

To install as a `kubectl` plugin (enables `kubectl multi-logs`):

```bash
cp kubectl-multi-logs /usr/local/bin/kubectl-multi-logs
```

## Usage

```
kubectl-multi-logs [-n namespace] [-s since] [-g pattern] [-e] [-o output_file] <app1> <app2> ...
```

### Flags

| Flag | Description | Default |
|---|---|---|
| `-n` | Kubernetes namespace | current context |
| `-s` | Show historical logs since (e.g. `10m`, `1h`, `2d`) | follow (real-time) |
| `-g` | Filter log lines — case-insensitive, supports `\|` for OR | none |
| `-e` | Filter for `ERROR\|WARN\|Exception\|failed\|error` | off |
| `-o` | Output file name | `tail_multiple_logs_data.log` |

### Examples

```bash
# Follow live logs from two apps
kubectl-multi-logs -n production agent-service tez-api

# Historical logs from the last 30 minutes, errors only
kubectl-multi-logs -n production -s 30m -e agent-service

# Filter for a specific request ID across three services
kubectl-multi-logs -n staging -g "req-abc123" api gateway worker

# Save to a custom file
kubectl-multi-logs -n production -o my-debug.log agent-service
```

## How it works

1. **Phase 1 — Find pods**: Queries `kubectl get pods -l app=<name>` for each app concurrently. A scroll-and-advance progress display shows each app as its pods are resolved.

2. **Phase 2 — Fetch containers**: Queries `kubectl get pod` for each pod concurrently to retrieve container names. Same scroll-and-advance display.

3. **Phase 3 — Stream logs**: Launches one goroutine per pod/container. Each goroutine runs `kubectl logs` and writes lines to the output file (protected by a mutex). A foreground monitor redraws live line counts using atomic reads — no temp files, no polling latency.

All output is written to the log file. Press `Ctrl+C` to stop all streams cleanly.

## Output file

Lines are prefixed with `[pod:container]` for easy grepping:

```
[agent-service-7d4b9c-xkp2q:agent] 2026-03-16T10:00:01Z INFO starting
[tez-api-6f8d2a-mn3rt:api]         2026-03-16T10:00:02Z INFO listening on :8080
```

The file is written relative to the binary's location.

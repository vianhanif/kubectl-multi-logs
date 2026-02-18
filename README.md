# kubectl-multi-logs

A Bash script to efficiently collect and tail logs from multiple Kubernetes pods across different applications. Simplify log monitoring by aggregating logs from all replicas into a single, organized output—perfect for debugging distributed systems.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

## Features

- **Parallel Log Collection**: Fetches logs from multiple pods concurrently for faster processing.
- **Replica Coverage**: Automatically includes all replicas of specified apps.
- **Real-Time & Historical Modes**: Tail logs live or retrieve historical logs from a specified time.
- **Flexible Filtering**: Filter by patterns or focus on error logs (ERROR, WARN, etc.).
- **Organized Output**: Prefixes logs with `[pod:container]` for easy identification.
- **Quiet Mode**: Save logs to a file without terminal output.
- **Progress Indicators**: Visual feedback with spinners and counters.
- **Tree-Table Summary**: Displays a clear overview of apps and containers being monitored.
- **Cross-Platform**: Compatible with bash/zsh; works on macOS/Linux.

## Benefits

- **No Multiple Terminal Tabs**: Aggregate logs into one stream or file, reducing clutter and boosting productivity.
- **Copilot-Ready Debugging**: Output files can be analyzed by AI tools like GitHub Copilot for correlating issues across pods and replicas.
- **Comprehensive Coverage**: Ensures no pod or replica is missed during troubleshooting.
- **Speed & Efficiency**: Parallel processing cuts down collection time for large deployments.
- **User-Friendly**: Progress spinners, organized prefixes, and summaries enhance the experience.
- **Flexible Workflows**: Supports background saving, filtering, and integration with shell aliases.

## Installation

1. Clone or download the repository:
   ```bash
   git clone https://github.com/yourusername/kubectl-multi-logs.git
   cd kubectl-multi-logs
   ```

2. Make the script executable:
   ```bash
   chmod +x tail_multiple_logs.sh
   ```

3. Ensure `kubectl` is installed and configured for your cluster.

### Optional: Add to Shell Profile

Add an alias to your `~/.zshrc` (or `~/.bashrc`) for quick access:

```bash
app_logs () {
    ~/Documents/tail_multiple_logs.sh "$@" -- app-1 app-2
}
```

Reload your shell: `source ~/.zshrc`. Now use `app_logs` instead of the full path.

## Usage

```bash
./tail_multiple_logs.sh [-n namespace] [-s since] [-g grep_pattern] [-e] [-o [output_file]] <app1> <app2> ...
```

### Options

- `-n namespace`: Kubernetes namespace (default: current context).
- `-s since`: Historical logs from relative time (e.g., `10m`, `1h`, `1d`). Omits for real-time tailing.
- `-g pattern`: Filter logs containing the pattern (case-insensitive).
- `-e`: Filter for error-related logs (ERROR, WARN, Exception, failed, error).
- `-o [output_file]`: Output file name (default: `tail_multiple_logs_data.log`) and enable quiet mode.

## Examples

- Tail real-time logs from `agent-service` and `tez-api`:
  ```bash
  ./tail_multiple_logs.sh agent-service tez-api
  ```

- Get last 30 minutes of logs for `agent-service` and save to file:
  ```bash
  ./tail_multiple_logs.sh -s 30m -o agent-service
  ```

- Filter errors from `tez-api` in `production` namespace:
  ```bash
  ./tail_multiple_logs.sh -n production -e tez-api
  ```

- Search for "ERROR" across apps and save to custom file:
  ```bash
  ./tail_multiple_logs.sh -g ERROR -o custom_logs.log agent-service tez-api
  ```

## Example Output

### Terminal Output

When running the script, you'll see progress feedback followed by the collected logs:

```
$ ./tail_multiple_logs.sh -s 10m agent-service tez-api

 [/] Finding pods... (2/2)
Fetching containers for pods...
Showing historical logs from 10m to now for 5 pods and their containers.
Building summary of apps and containers...
App: agent-service
  - agent-service
  - nginx
App: tez-api
  - tez-api
  - celery-worker
  - nginx
Starting logs for pod: tez-api-6f8b9c7d4-xk2mn (5/5)
 [|] Collecting logs...

Historical logs collection completed. Logs saved to: /path/to/tail_multiple_logs_data.log
```

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

> **Tip**: This output file can be shared with GitHub Copilot or other AI tools to analyze and correlate errors across pods—no need to manually cross-reference logs from different terminals.

## Getting Pod App List

To discover available app names in your cluster, run:

```bash
kubectl get pods -o jsonpath='{.items[*].metadata.labels.app}' | tr ' ' '\n' | sort | uniq
```

This lists unique app labels from all pods, helping you identify apps to monitor.

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
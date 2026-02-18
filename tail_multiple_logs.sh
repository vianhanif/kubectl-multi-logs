#!/bin/bash

# Script to collectively tail logs from multiple Kubernetes pods by app name
# Usage: ./tail_multiple_logs.sh [-n namespace] [-s since] [-g grep_pattern] [-e] [-o [output_file]] <app1> <app2> ...
# Note: Using -s (since) shows historical logs and exits; without -s, it follows logs in real-time.
# -e: Filter for error-related logs (ERROR, WARN, Exception, failed, error)
# -o [output_file]: Specify output file name (default: tail_multiple_logs_data.log) and enable quiet mode

namespace=""
since=""
grep_pattern=""
errors=""
output_file="tail_multiple_logs_data.log"
output_specified=false

# Parse options
while getopts "n:s:g:eo::" opt; do
    case $opt in
        n) namespace="$OPTARG" ;;
        s) since="$OPTARG" ;;
        g) grep_pattern="$OPTARG" ;;
        e) errors="true" ;;
        o) output_specified=true
           if [ -n "$OPTARG" ] && [ "$OPTARG" != "--" ]; then output_file="$OPTARG"; fi ;;
        *) echo "Usage: $0 [-n namespace] [-s since] [-g grep_pattern] [-e] [-o [output_file]] <app1> <app2> ..."
           echo "Note: -s shows historical logs from the specified time to now; without -s, follows logs in real-time."
           echo "      -e filters for error-related logs (ERROR, WARN, Exception, failed, error)"
           echo "      -o [output_file] specifies output file name (default: tail_multiple_logs_data.log) and enables quiet mode"
           exit 1 ;;
    esac
done
shift $((OPTIND-1))

# Get script directory
script_dir=$(cd "$(dirname "$0")" && pwd)
output_file_path="${script_dir}/${output_file}"

# Determine if quiet mode (no terminal output)
if [ "$output_specified" = true ]; then
    quiet_mode=true
else
    quiet_mode=false
fi

# Handle errors filter
if [ "$errors" = "true" ]; then
    if [ -n "$grep_pattern" ]; then
        grep_pattern="$grep_pattern|ERROR|WARN|Exception|failed|error"
    else
        grep_pattern="ERROR|WARN|Exception|failed|error"
    fi
fi

# Spinner function for loading animation
spinner() {
    local spinstr='|/-\'

    if [ $# -eq 1 ]; then
        local message=$1
        local delay=0.1
        while true; do
            local temp=${spinstr#?}
            printf " [%c] $message " "$spinstr"
            spinstr=$temp${spinstr%"$temp"}
            sleep $delay
            printf "\r"
        done
    elif [ $# -eq 2 ]; then
        local total=$1
        local file=$2
        local delay=0.1
        while true; do
            local temp=${spinstr#?}
            local current=$(wc -l < "$file" 2>/dev/null || echo 0)
            printf " [%c] Finding pods... ($current/$total) " "$spinstr"
            spinstr=$temp${spinstr%"$temp"}
            sleep $delay
            printf "\r"
        done
    fi
}

# Determine if we should follow logs
if [ -n "$since" ]; then
    follow=""
else
    follow="-f"
fi

if [ $# -eq 0 ]; then
    echo "Usage: $0 [-n namespace] [-s since] [-g grep_pattern] [-e] [-o [output_file]] <app1> <app2> ..."
    echo "Options:"
    echo "  -n namespace       Specify Kubernetes namespace (default: current)"
    echo "  -s since           Show logs since a relative time (e.g., 10m, 1h, 1d)"
    echo "  -g pattern         Filter logs containing the pattern (case-insensitive)"
    echo "  -e                 Filter for error-related logs (ERROR, WARN, Exception, failed, error)"
    echo "  -o [output_file]   Specify output file name (default: tail_multiple_logs_data.log) and enable quiet mode"
    echo "Example: $0 -n production -s 5m -g ERROR agent-service tez-api"
    exit 1
fi

# Collect all pod names in parallel (unchanged)
echo "Finding pods for apps..." >&2
counter_file=$(mktemp)
( spinner $# $counter_file ) 2>/dev/null & spinner_pid=$!
disown $spinner_pid
pods=()
temp_file=$(mktemp)
pids=()

for app in "$@"; do
    (
        app_pods=$(kubectl get pods -l app="$app" ${namespace:+-n "$namespace"} --no-headers -o custom-columns=":metadata.name" 2>/dev/null)
        if [ -z "$app_pods" ]; then
            echo "Warning: No pods found for app '$app'" >&2
        else
            for pod in $app_pods; do
                echo "$app|$pod" >> "$temp_file"
            done
        fi
        echo 1 >> "$counter_file"
    ) &
    pids+=($!)
done

# Wait for all background jobs
for pid in "${pids[@]}"; do
    wait "$pid"
done

# Read pods from temp file
while IFS='|' read -r app pod; do
    pods+=("$pod")
done < "$temp_file"
rm "$counter_file"

# Stop spinner
if [ -n "$spinner_pid" ]; then
    kill $spinner_pid 2>/dev/null
    printf '\r'
    tput el 2>/dev/null || printf '%60s\r' ''
fi

if [ ${#pods[@]} -eq 0 ]; then
    echo "No pods found for the specified apps. Exiting."
    exit 1
fi

echo "Fetching containers for pods..." >&2

# NEW: Parallelize fetching containers for all pods
temp_containers_file=$(mktemp)
concurrency_limit=10  # Limit parallel kubectl calls to avoid overwhelming the cluster
pids=()
active_jobs=0

for pod in "${pods[@]}"; do
    (
        containers=$(kubectl get pod "$pod" ${namespace:+-n "$namespace"} -o jsonpath='{.spec.containers[*].name}' 2>/dev/null)
        if [ -z "$containers" ]; then
            echo "Warning: No containers found for pod $pod" >&2
        else
            echo "$pod|$containers" >> "$temp_containers_file"
        fi
    ) &
    pids+=($!)
    ((active_jobs++))
    
    # Throttle concurrency
    if [ "$active_jobs" -ge "$concurrency_limit" ]; then
        wait "${pids[0]}"  # Wait for the first job to finish
        pids=("${pids[@]:1}")  # Remove it from the array
        ((active_jobs--))
    fi
done

# Wait for remaining jobs
for pid in "${pids[@]}"; do
    wait "$pid"
done

if [ ${#pods[@]} -eq 0 ]; then
    echo "No pods found for the specified apps. Exiting."
    exit 1
fi

# Truncate the output file
: > "$output_file_path"

if [ -n "$since" ]; then
    echo "Showing historical logs from $since to now for ${#pods[@]} pods and their containers."
    
    echo "Building summary of apps and containers..."
    # Display tree-table of apps and containers
    app_containers_file=$(mktemp)
    while IFS='|' read -r app pod; do
        containers=$(grep "^$pod|" "$temp_containers_file" | cut -d'|' -f2)
        echo "$app|$containers" >> "$app_containers_file"
    done < "$temp_file"
    
    sort -u "$app_containers_file" | while IFS='|' read -r app conts; do
        echo "App: $app"
        for c in $conts; do
            echo "  - $c"
        done
    done
    rm "$app_containers_file"
else
    echo "Starting log tailing for ${#pods[@]} pods and their containers."
fi
rm "$temp_file"
if [ "$quiet_mode" = true ]; then
    echo "Logs are being saved to: $output_file_path"
fi
echo "Press Ctrl+C to stop all log streams"
echo "----------------------------------------"

# Function to handle cleanup on exit
cleanup() {
    echo ""
    echo "Stopping all log streams..."
    if [ -n "$spinner_pid" ]; then
        kill $spinner_pid 2>/dev/null
    fi
    # Kill any remaining background processes
    for pid in "${pids[@]}"; do
        kill $pid 2>/dev/null
    done
    kill 0
    exit 0
}

# Set trap to cleanup background processes
trap cleanup SIGINT SIGTERM

# Define colors using tput for better terminal compatibility
colors=()
if command -v tput >/dev/null 2>&1 && [ -n "$TERM" ] && [ "$TERM" != "dumb" ]; then
    colors=(
        "$(tput setaf 1)"  # red
        "$(tput setaf 2)"  # green
        "$(tput setaf 3)"  # yellow
        "$(tput setaf 4)"  # blue
        "$(tput setaf 5)"  # magenta
        "$(tput setaf 6)"  # cyan
    )
    reset="$(tput sgr0)"
else
    # Fallback to ANSI codes if tput not available
    colors=(
        "\033[31m"
        "\033[32m"
        "\033[33m"
        "\033[34m"
        "\033[35m"
        "\033[36m"
    )
    reset="\033[0m"
fi
color_index=0

# UPDATED: Start tailing logs for each pod and its containers (now all at once after containers are fetched)
total_pods=${#pods[@]}
current_pod=0
for pod in "${pods[@]}"; do
    containers=$(grep "^$pod|" "$temp_containers_file" | cut -d'|' -f2)
    if [ -z "$containers" ]; then
        continue  # Skip if no containers (warning already printed)
    fi
    ((current_pod++))
    printf "Starting logs for pod: $pod ($current_pod/$total_pods)                                                                                                    \r"
    for container in $containers; do
        color=${colors[$color_index % ${#colors[@]}]}
        if [ -n "$grep_pattern" ]; then
            if [ "$quiet_mode" = true ]; then
                kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} | grep -i "$grep_pattern" | sed "s/^/[$pod:$container] /" >> "$output_file_path" &
            else
                kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} | grep -i "$grep_pattern" | sed "s/^/[$pod:$container] /" | tee -a "$output_file_path" | sed "s/^\[$pod:$container\] /${color}[$pod:$container]${reset} /" &
            fi
        else
            if [ "$quiet_mode" = true ]; then
                kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} | sed "s/^/[$pod:$container] /" >> "$output_file_path" &
            else
                kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} | sed "s/^/[$pod:$container] /" | tee -a "$output_file_path" | sed "s/^\[$pod:$container\] /${color}[$pod:$container]${reset} /" &
            fi
        fi
        ((color_index++))
    done
done
printf "\n"

# Clean up temp file
rm "$temp_containers_file"

# Start spinner for historical logs collection
if [ -n "$since" ]; then
    ( spinner "Collecting logs..." ) 2>/dev/null & spinner_pid=$!
    disown $spinner_pid
fi

# Wait for all background processes
wait

# Stop spinner
if [ -n "$spinner_pid" ]; then
    kill $spinner_pid 2>/dev/null
    printf '\r'
    tput el 2>/dev/null || printf '%60s\r' ''
fi

# Show completion summary for historical logs
if [ -n "$since" ]; then
    echo ""
    echo "Historical logs collection completed. Logs saved to: $output_file_path"
fi
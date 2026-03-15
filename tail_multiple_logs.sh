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
# Temporary directory for per-stream status tracking
status_dir=$(mktemp -d)

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
# Scroll-and-advance monitor for setup phases.
# Items are printed permanently (one line each) as they complete;
# a single spinner line at the bottom is overwritten with \r.
phase_monitor() {
    local spins=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
    local spin_idx=0
    local -a items=("$@")
    local n=${#items[@]}
    local done_count=0

    while [ $done_count -lt $n ]; do
        local pending=0 cols spin_char
        cols=$(tput cols 2>/dev/null || stty size 2>/dev/null | awk '{print $2}' || echo 80)
        spin_char="${spins[$spin_idx]}"

        for item in "${items[@]}"; do
            local safe
            safe=$(printf '%s' "$item" | tr ':/' '__')
            [ -f "${status_dir}/${safe}.printed" ] && continue

            if [ -f "${status_dir}/${safe}.done" ]; then
                local result
                result=$(cat "${status_dir}/${safe}.result" 2>/dev/null || echo "done")
                local left_vis="  x  ${item}"
                local pad=$(( cols - ${#left_vis} - ${#result} - 2 ))
                [ $pad -lt 1 ] && pad=1
                printf "\r\033[K  \033[32m✔\033[0m  %s%${pad}s\033[2m%s\033[0m\n" \
                    "$item" "" "$result"
                touch "${status_dir}/${safe}.printed"
                ((done_count++))
            else
                ((pending++))
            fi
        done

        if [ $done_count -lt $n ]; then
            printf "  \033[33m%s\033[0m  Waiting for %d more...\033[K\r" "$spin_char" "$pending"
            sleep 0.1
            spin_idx=$(( (spin_idx + 1) % ${#spins[@]} ))
        fi
    done
    printf "\r\033[K"
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

# Phase 1: Find all pods for given apps in parallel
echo "Finding pods for apps..."
pods=()
temp_file=$(mktemp)
pids=()

for app in "$@"; do
    safe=$(printf '%s' "$app" | tr ':/' '__')
    (
        app_pods=$(kubectl get pods -l app="$app" ${namespace:+-n "$namespace"} --no-headers -o custom-columns=":metadata.name" 2>/dev/null)
        if [ -z "$app_pods" ]; then
            printf "no pods found" > "${status_dir}/${safe}.result"
        else
            cnt=0
            for pod in $app_pods; do
                echo "$app|$pod" >> "$temp_file"
                ((cnt++))
            done
            printf "%d pod(s)" "$cnt" > "${status_dir}/${safe}.result"
        fi
        printf "done\n" > "${status_dir}/${safe}.done"
    ) &
    pids+=($!)
done

phase_monitor "$@"
for pid in "${pids[@]}"; do wait "$pid"; done

while IFS='|' read -r app pod; do
    pods+=("$pod")
done < "$temp_file"

if [ ${#pods[@]} -eq 0 ]; then
    echo "No pods found for the specified apps. Exiting."
    exit 1
fi

# Phase 2: Fetch containers for all pods in parallel
echo "Fetching containers for pods..."
temp_containers_file=$(mktemp)
pids=()

for pod in "${pods[@]}"; do
    safe=$(printf '%s' "$pod" | tr ':/' '__')
    (
        containers=$(kubectl get pod "$pod" ${namespace:+-n "$namespace"} -o jsonpath='{.spec.containers[*].name}' 2>/dev/null)
        if [ -z "$containers" ]; then
            printf "no containers" > "${status_dir}/${safe}.result"
        else
            cnt=$(echo "$containers" | wc -w | tr -d ' ')
            printf "%s container(s)" "$cnt" > "${status_dir}/${safe}.result"
            echo "$pod|$containers" >> "$temp_containers_file"
        fi
        printf "done\n" > "${status_dir}/${safe}.done"
    ) &
    pids+=($!)
done

phase_monitor "${pods[@]}"
for pid in "${pids[@]}"; do wait "$pid"; done

# Truncate the output file
: > "$output_file_path"

if [ -n "$since" ]; then
    echo "Showing historical logs from $since to now for ${#pods[@]} pods and their containers."
else
    echo "Starting log tailing for ${#pods[@]} pods and their containers."
fi
echo "Logs are being saved to: $output_file_path"
echo "Press Ctrl+C to stop all log streams"
echo "----------------------------------------"

# Function to handle cleanup on exit
cleanup() {
    printf '\n'
    echo "Stopping all log streams..."
    rm -rf "$status_dir" 2>/dev/null
    kill 0
    exit 0
}

# Set trap to cleanup background processes
trap cleanup SIGINT SIGTERM

# Function: stream a pod/container's logs to file, tracking line count for the status display
stream_log_with_status() {
    local pod="$1"
    local container="$2"
    local label="${pod}:${container}"
    local safe
    safe=$(echo "$label" | tr ':/' '__')
    local count_file="${status_dir}/${safe}.count"
    local done_file="${status_dir}/${safe}.done"
    printf "0\n" > "$count_file"
    local n=0
    while IFS= read -r line; do
        printf "[%s] %s\n" "$label" "$line" >> "$output_file_path"
        n=$((n + 1))
        printf "%d\n" "$n" > "$count_file"
    done < <(
        if [ -n "$grep_pattern" ]; then
            kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} 2>/dev/null | grep -i "$grep_pattern"
        else
            kubectl logs $follow ${since:+--since="$since"} "$pod" -c "$container" ${namespace:+-n "$namespace"} 2>/dev/null
        fi
    )
    printf "done\n" > "$done_file"
}

# Function: scroll-and-advance log status display.
# Items are printed permanently as they complete, grouped by app.
# A single spinner line at the bottom is overwritten with \r until all streams finish.
display_status_monitor() {
    local spins=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
    local spin_idx=0

    # Sort items by app then pod so same-app streams complete adjacently
    local -a sorted_items=()
    while IFS= read -r _si; do
        sorted_items+=("$_si")
    done < <(printf '%s\n' "$@" | sort -t'|' -k1,1 -k2,2)

    local n=${#sorted_items[@]}
    local done_count=0

    while [ $done_count -lt $n ]; do
        local cols spin_char _item
        cols=$(tput cols 2>/dev/null || stty size 2>/dev/null | awk '{print $2}' || echo 80)
        spin_char="${spins[$spin_idx]}"

        for _item in "${sorted_items[@]}"; do
            IFS='|' read -r _app _pod _container <<< "$_item"
            local _safe
            _safe=$(printf '%s' "${_pod}:${_container}" | tr ':/' '__')
            [ -f "${status_dir}/${_safe}.printed" ] && continue

            if [ -f "${status_dir}/${_safe}.done" ]; then
                local _count
                _count=$(cat "${status_dir}/${_safe}.count" 2>/dev/null || echo 0)

                # Print app header the first time we see a completion for this app
                local _app_safe
                _app_safe=$(printf '%s' "$_app" | tr ' /' '__')
                if [ ! -f "${status_dir}/appheader_${_app_safe}.printed" ]; then
                    printf "\r\033[K  \033[1m%s\033[0m\n" "$_app"
                    touch "${status_dir}/appheader_${_app_safe}.printed"
                fi

                local _left_vis="    x [${_pod}] ${_container}"
                local _status_word
                [ -n "$since" ] && _status_word="Collected " || _status_word="Following  "
                local _right="${_status_word}  ${_count} lines"
                local _pad=$(( cols - ${#_left_vis} - ${#_right} ))
                [ $_pad -lt 1 ] && _pad=1

                printf "\r\033[K    \033[32m✔\033[0m [\033[36m%s\033[0m] %s%${_pad}s\033[2m%s\033[0m\n" \
                    "$_pod" "$_container" "" "$_right"
                touch "${status_dir}/${_safe}.printed"
                ((done_count++))
            fi
        done

        if [ $done_count -lt $n ]; then
            local _pending=$(( n - done_count ))
            [ -n "$since" ] && _verb="Collecting" || _verb="Following "
            printf "  \033[33m%s\033[0m  %s logs... (%d/%d streams done)\033[K\r" \
                "$spin_char" "$_verb" "$done_count" "$n"
            sleep 0.1
            spin_idx=$(( (spin_idx + 1) % ${#spins[@]} ))
        fi
    done
    printf "\r\033[K"
}

# Show a spinner while stream processes are being prepared
( spins=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
  i=0
  while true; do
    printf "  \033[33m%s\033[0m  Preparing streams...\033[K\r" "${spins[$i]}"
    sleep 0.1
    i=$(( (i + 1) % 10 ))
  done ) & _prep_pid=$!

# Start a background log-stream process for every pod/container, preserving app association
stream_labels=()
while IFS='|' read -r app pod; do
    containers=$(grep "^$pod|" "$temp_containers_file" | cut -d'|' -f2)
    if [ -z "$containers" ]; then
        continue
    fi
    for container in $containers; do
        stream_labels+=("${app}|${pod}|${container}")
        stream_log_with_status "$pod" "$container" &
    done
done < "$temp_file"
rm "$temp_file"

# Clean up temp containers file
rm "$temp_containers_file"

# Stop prep spinner and clear its line before monitor takes over
kill $_prep_pid 2>/dev/null
printf "\033[K"

# Run the brew-like status display in the FOREGROUND (blocks until all streams complete)
display_status_monitor "${stream_labels[@]}"

# Wait for any remaining background stream processes
wait

# Clean up status tracking directory
rm -rf "$status_dir"

# Show completion summary for historical logs
if [ -n "$since" ]; then
    echo ""
    echo "Historical logs collection completed. Logs saved to: $output_file_path"
fi
#!/bin/sh
set -eu

usage() {
    printf 'Usage: %s {start|stop|restart|status}\n' "$0"
}

if [ "$#" -ne 1 ]; then
    usage >&2
    exit 2
fi
case "$1" in
    start|stop|restart|status) action=$1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
esac
if ! ps -p "$$" -o pid= >/dev/null 2>&1; then
    printf 'Cannot query processes with ps\n' >&2
    exit 1
fi

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
service_bin="$project_dir/bin/codex-app-server-proxy"
account_uid=$(id -ru)
case "$(uname -s)" in
    Darwin)
        account_home=$(dscacheutil -q user -a uid "$account_uid" | awk '/^dir: / { sub(/^dir: /, ""); print; exit }')
        ;;
    Linux)
        account_home=$(getent passwd "$account_uid" | cut -d: -f6)
        ;;
    *) printf 'Unsupported operating system\n' >&2; exit 1 ;;
esac
case "$account_home" in
    /*) ;;
    *) printf 'Cannot resolve the system account home directory\n' >&2; exit 1 ;;
esac

proxy_dir="$account_home/.codex-proxy"
pid_file="$proxy_dir/app-server.pid"
socket_file="$proxy_dir/app-server.sock"
stderr_file="$proxy_dir/app-server-daemon.stderr"
if [ -L "$proxy_dir" ]; then
    printf 'State directory must not be a symlink: %s\n' "$proxy_dir" >&2
    exit 1
fi

is_service() {
    service_args=$(ps -ww -p "$1" -o args= 2>/dev/null || true)
    case "$service_args" in
        "$service_bin"|"$service_bin --daemon"|"$service_bin --connect") return 0 ;;
        *) return 1 ;;
    esac
}

daemon_running() {
    [ -f "$pid_file" ] && [ ! -L "$pid_file" ] || return 1
    daemon_pid=$(cat "$pid_file")
    case "$daemon_pid" in ''|*[!0-9]*) return 1 ;; esac
    [ "$daemon_pid" -gt 1 ] || return 1
    [ "$(ps -ww -p "$daemon_pid" -o args= 2>/dev/null || true)" = "$service_bin --daemon" ]
}

start_service() {
    if daemon_running; then
        printf 'codex-app-server-proxy is already running (PID %s)\n' "$daemon_pid"
        return
    fi
    if [ ! -f "$service_bin" ] || [ ! -x "$service_bin" ]; then
        printf 'Build the service first with %s/build.sh\n' "$project_dir" >&2
        return 1
    fi
    umask 077
    mkdir -p "$proxy_dir"
    chmod 700 "$proxy_dir"
    if [ -L "$stderr_file" ] || { [ -e "$stderr_file" ] && [ ! -f "$stderr_file" ]; }; then
        printf 'Daemon stderr path must be a regular file\n' >&2
        return 1
    fi
    touch "$stderr_file"
    chmod 600 "$stderr_file"
    nohup "$service_bin" --daemon </dev/null >/dev/null 2>>"$stderr_file" &
    started_pid=$!
    attempts=0
    while [ "$attempts" -lt 50 ]; do
        if daemon_running && [ -S "$socket_file" ]; then
            printf 'Started codex-app-server-proxy (PID %s)\n' "$daemon_pid"
            return
        fi
        if ! kill -0 "$started_pid" 2>/dev/null; then
            wait "$started_pid" 2>/dev/null || true
            printf 'codex-app-server-proxy failed to start; see %s\n' "$stderr_file" >&2
            return 1
        fi
        sleep 0.1
        attempts=$((attempts + 1))
    done
    kill -s TERM "$started_pid" 2>/dev/null || true
    printf 'Timed out starting codex-app-server-proxy; see %s\n' "$stderr_file" >&2
    return 1
}

wait_for_shutdown() {
    attempts=0
    while [ "$attempts" -lt "$1" ]; do
        remaining=""
        for service_pid in $service_pids; do
            if is_service "$service_pid"; then remaining="$remaining $service_pid"; fi
        done
        live_groups=$(ps -axo pgid=,stat= | awk -v groups="$child_groups" '
            BEGIN { n=split(groups, ids, "\n"); for (i=1; i<=n; i++) group[ids[i]]=1 }
            group[$1] && substr($2,1,1) != "Z" { print $1; exit }
        ')
        if [ -z "$remaining" ] && [ -z "$live_groups" ]; then return 0; fi
        sleep 0.1
        attempts=$((attempts + 1))
    done
    return 1
}

stop_service() {
    # Include standalone stdio sessions from this installation, not only workers
    # of the daemon. Guard processes retain their locks until native children die.
    candidates=$(ps -ww -axo pid=,uid=,args= | awk -v uid="$account_uid" -v binary="$service_bin" '
        $2 == uid { pid=$1; sub(/^[[:space:]]*[0-9]+[[:space:]]+[0-9]+[[:space:]]+/, "");
            if ($0 == binary || $0 == binary " --daemon" || $0 == binary " --connect") print pid }
    ')
    service_pids=""
    for service_pid in $candidates; do
        if is_service "$service_pid"; then
            service_pids="$service_pids $service_pid"
        fi
    done
    if [ -z "$service_pids" ]; then
        printf 'codex-app-server-proxy is stopped\n'
        return
    fi
    # Save descendant process groups before graceful shutdown removes parents.
    child_groups=$(ps -axo pid=,ppid=,pgid=,uid= | awk -v roots="$service_pids" -v uid="$account_uid" '
        BEGIN { n=split(roots, ids, " "); for (i=1; i<=n; i++) if (ids[i] != "") root[ids[i]]=seen[ids[i]]=1 }
        $4 == uid { parent[$1]=$2; group[$1]=$3 }
        END {
            changed=1
            while (changed) { changed=0; for (pid in parent) if (!seen[pid] && seen[parent[pid]]) { seen[pid]=1; changed=1 } }
            for (pid in seen) if (!root[pid] && group[pid] == pid && pid > 1) print pid
        }
    ')
    for service_pid in $service_pids; do
        kill -s TERM "$service_pid" 2>/dev/null || true
    done
    if wait_for_shutdown 50; then
        printf 'Stopped codex-app-server-proxy and its codex app-server processes\n'
        return
    fi
    for child_group in $child_groups; do
        kill -s KILL -- "-$child_group" 2>/dev/null || true
    done
    for service_pid in $service_pids; do
        if is_service "$service_pid"; then kill -s KILL "$service_pid" 2>/dev/null || true; fi
    done
    if wait_for_shutdown 20; then
        printf 'Forced shutdown after waiting 5 seconds\n' >&2
    else
        printf 'Some codex app-server processes did not exit\n' >&2
        return 1
    fi
}

case "$action" in
    start) start_service ;;
    stop) stop_service ;;
    restart) stop_service; start_service ;;
    status)
        if daemon_running; then
            printf 'codex-app-server-proxy is running (PID %s)\n' "$daemon_pid"
        else
            printf 'codex-app-server-proxy is stopped\n'
            exit 3
        fi
        ;;
esac

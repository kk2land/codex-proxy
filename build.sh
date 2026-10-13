#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$project_dir"

if [ "$(go env GOOS)" = "linux" ]; then
    export CGO_ENABLED=1
fi

mkdir -p bin
go build -o bin/codex ./cmd/codex
printf 'Built %s/bin/codex\n' "$project_dir"
go build -o bin/claude ./cmd/claude
printf 'Built %s/bin/claude\n' "$project_dir"

case "$(uname -s)" in
    Darwin)
        account_home=$(dscacheutil -q user -a uid "$(id -ru)" | awk '/^dir: / { sub(/^dir: /, ""); print; exit }')
        ;;
    Linux)
        account_home=$(getent passwd "$(id -ru)" | cut -d: -f6)
        ;;
    *)
        printf 'Unsupported operating system\n' >&2
        exit 1
        ;;
esac

case "$account_home" in
    /*) ;;
    *)
        printf 'Cannot resolve the system account home directory\n' >&2
        exit 1
        ;;
esac

proxy_dir="$account_home/.codex-proxy"
if [ -L "$proxy_dir" ]; then
    printf 'Configuration directory must not be a symlink: %s\n' "$proxy_dir" >&2
    exit 1
fi

umask 077
mkdir -p "$proxy_dir"
chmod 700 "$proxy_dir"
if [ ! -e "$proxy_dir/env.example" ] && [ ! -L "$proxy_dir/env.example" ]; then
    cp env.example "$proxy_dir/env.example"
fi
printf 'Configuration template: %s/env.example\n' "$proxy_dir"

claude_proxy_dir="$account_home/.claude-proxy"
if [ -L "$claude_proxy_dir" ]; then
    printf 'Configuration directory must not be a symlink: %s\n' "$claude_proxy_dir" >&2
    exit 1
fi
mkdir -p "$claude_proxy_dir"
chmod 700 "$claude_proxy_dir"
if [ ! -e "$claude_proxy_dir/env.example" ] && [ ! -L "$claude_proxy_dir/env.example" ]; then
    cp claude.env.example "$claude_proxy_dir/env.example"
fi
printf 'Configuration template: %s/env.example\n' "$claude_proxy_dir"

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

#!/bin/sh
# Run local plugin tests beside their packages in a disposable source workspace.
set -eu
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ "$#" -eq 0 ]; then set -- test ./...; fi
case "$1" in test|vet) ;; *) echo "Usage: scripts/test.sh [test|vet] [go arguments...]" >&2; exit 2 ;; esac
for suite in plugins pluginruntime; do
    if [ ! -d "$project_dir/tests/$suite" ]; then
        echo "Local tests/$suite is missing (tests/ is intentionally ignored by Git)." >&2
        exit 1
    fi
    found=false
    for test_file in "$project_dir/tests/$suite/"*_test.go; do
        if [ -f "$test_file" ]; then found=true; break; fi
    done
    if [ "$found" = false ]; then echo "No local tests in tests/$suite." >&2; exit 1; fi
done
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/vk2tg-tests.XXXXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
cp "$project_dir/go.mod" "$project_dir/go.sum" "$project_dir/"*.go "$work_dir/"
mkdir -p "$work_dir/plugins" "$work_dir/internal/pluginruntime"
cp "$project_dir/plugins/"*.go "$work_dir/plugins/"
cp "$project_dir/internal/pluginruntime/"*.go "$work_dir/internal/pluginruntime/"
cp "$project_dir/tests/plugins/"*_test.go "$work_dir/plugins/"
cp "$project_dir/tests/pluginruntime/"*_test.go "$work_dir/internal/pluginruntime/"
cd "$work_dir"
go "$@"

#!/bin/sh
# Compatibility wrapper for the standard Go test and vet commands.
set -eu
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ "$#" -eq 0 ]; then set -- test ./...; fi
case "$1" in test|vet) ;; *) echo "Usage: scripts/test.sh [test|vet] [go arguments...]" >&2; exit 2 ;; esac
cd "$project_dir"
exec go "$@"

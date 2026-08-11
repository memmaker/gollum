#!/bin/sh
set -e
# Build without changing directory: crash looks for ./llm_model.rec in the
# working directory, so a cd here would silently switch config files.
go build -C "$(dirname "$0")/crash" -o "$(cd "$(dirname "$0")" && pwd)/crash-bin" .
exec "$(cd "$(dirname "$0")" && pwd)/crash-bin" "$@"

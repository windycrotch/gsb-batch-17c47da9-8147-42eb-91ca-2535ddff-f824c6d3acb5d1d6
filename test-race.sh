#!/usr/bin/env bash
# Reproducible acceptance run for goff on the Windows evaluation machine.
#
# Plain `go test -race ./...` also works from this working tree: the cgo
# directives in goff.go use the prepared C:/ffmpeg-dl tree on Windows, and the
# five av*.dll runtime shims live next to this package. This script additionally
# wires up the MSYS2 pkg-config toolchain for setups that prefer that path, and
# falls back to whatever `go`/`gcc` are already on PATH.
set -euo pipefail

cd "$(dirname "$0")"

FFMPEG_ROOT="${FFMPEG_ROOT:-/c/ffmpeg-dl/441/ffmpeg-4.4.1-full_build-shared}"
PC_DIR="${PKG_CONFIG_DIR:-/c/ffmpeg-dl/441/pc}"
MSYS_BIN="${MSYS_BIN:-/c/msys64/ucrt64/bin}"

[ -d "$FFMPEG_ROOT/bin" ] && export PATH="$FFMPEG_ROOT/bin:$PATH"
[ -d "$MSYS_BIN" ] && export PATH="$MSYS_BIN:$PATH"
[ -d "$PC_DIR" ] && export PKG_CONFIG_PATH="$(cygpath -w "$PC_DIR" 2>/dev/null || echo "$PC_DIR")"
export CGO_ENABLED=1

exec go test -race -count=1 "$@" ./...

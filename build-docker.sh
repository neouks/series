#!/usr/bin/env bash
# Build the Linux executable for the Docker daemon's architecture, then the image.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"
command -v go >/dev/null || { echo '需要 Go >=1.26' >&2; exit 1; }
command -v npm >/dev/null || { echo '需要 Node.js 与 npm 构建前端' >&2; exit 1; }
arch="$(docker info --format '{{.Architecture}}')"
case "$arch" in
  amd64|x86_64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "不支持的 Docker 架构：$arch" >&2; exit 1 ;;
esac
SERIES_RELEASE=0 SERIES_PACKAGE=0 SERIES_OUTPUT="dist/$arch/series" \
  ./build.sh --target "linux/$arch"
docker compose build series

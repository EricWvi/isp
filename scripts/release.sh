#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

npm --prefix frontend ci
npm --prefix frontend run build
go test ./...
mkdir -p release
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags=-buildid= -o release/isp ./cmd/isp

echo "Built $repo_dir/release/isp"

#!/bin/bash
# Build script for WebMap
set -e

cd "$(dirname "$0")"
go build -o webmap cmd/webmap/main.go
echo "Build complete: ./webmap"
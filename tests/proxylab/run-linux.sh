#!/bin/sh
# Runs the lab tests as a Linux client inside the lab network (any host OS).
set -eu
cd "$(dirname "$0")"
arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64|arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac
bin=$(mktemp -d)/lab.test
(cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go test -c -o "$bin" ./tests/proxylab/)
LAB_TEST_BIN=$bin docker compose --profile test run --rm --build runner

#!/usr/bin/env bash
# Compare the current working tree with a fixed revision using the SAME harness.
# Requires the dedicated local Azurite described in reference/metrics.md,
# or explicitly configured live accounts (see that page for authorization).
set -euo pipefail

root=$(git rev-parse --show-toplevel)
baseline=${1:-fb40b8f4abee9bf64357593a744d730617924fa9}
mkdir -p "$root/.scratch"
output=$(mktemp -d "$root/.scratch/performance-compare.XXXXXX")
mkdir "$output/baseline"
git -C "$root" archive "$baseline" | tar -x -C "$output/baseline"
# Apply already accepted improvements to the reference when isolating a later
# change. Retain the exact patch alongside the binaries and measurement settings.
if [[ -n "${AZNET_MEASURE_BASELINE_PATCH:-}" ]]; then
    cp "$AZNET_MEASURE_BASELINE_PATCH" "$output/baseline.patch"
    GIT_CEILING_DIRECTORIES="$output" git -C "$output/baseline" apply "$output/baseline.patch"
fi
cp "$root/tests/performance/workload_test.go" "$output/baseline/tests/performance/workload_test.go"
(
    cd "$output/baseline"
    GOWORK=off go test -c -o "$output/before.test" ./tests/performance
)
(
    cd "$root"
    GOWORK=off go test -c -o "$output/after.test" ./tests/performance
)
git -C "$root" rev-parse "$baseline" > "$output/baseline-revision"
git -C "$root" rev-parse HEAD > "$output/candidate-revision"
git -C "$root" diff --binary > "$output/candidate.patch"
cp "$root/tests/performance/workload_test.go" "$output/harness.go"
go version > "$output/toolchain"
shasum -a 256 "$output/before.test" "$output/after.test" "$output/harness.go" > "$output/checksums"

export AZNET_MEASURE=1
export AZNET_MEASURE_STREAM_BYTES=${AZNET_MEASURE_STREAM_BYTES:-4194304}
export AZNET_MEASURE_WRITE_SIZE=${AZNET_MEASURE_WRITE_SIZE:-65536}
export AZNET_MEASURE_READ_SIZE=${AZNET_MEASURE_READ_SIZE:-$AZNET_MEASURE_WRITE_SIZE}
export AZNET_MEASURE_IDLE_MS=${AZNET_MEASURE_IDLE_MS:-1000}
filter=${AZNET_MEASURE_FILTER:-'^TestSDKWorkloadMeasurement$/az(blob|queue|table)/connections(1|4|16)$/(idle|interactive|stream|duplex|wake)$'}
printf 'stream_bytes=%s\nwrite_size=%s\nread_size=%s\nidle_ms=%s\nfilter=%s\n' \
    "$AZNET_MEASURE_STREAM_BYTES" "$AZNET_MEASURE_WRITE_SIZE" "$AZNET_MEASURE_READ_SIZE" "$AZNET_MEASURE_IDLE_MS" "$filter" > "$output/settings"
if [[ -n "${AZNET_MEASURE_LIVE_CONFIG:-}" ]]; then
    printf 'environment=azure\nazblob_account=%s\nazqueue_account=%s\naztable_account=%s\n' \
        "${AZNET_MEASURE_AZBLOB_ACCOUNT:-}" "${AZNET_MEASURE_AZQUEUE_ACCOUNT:-}" "${AZNET_MEASURE_AZTABLE_ACCOUNT:-}" >> "$output/settings"
else
    printf 'environment=azurite\n' >> "$output/settings"
fi

for round in 1 2 3; do
    variants="before after"
    if [[ "$round" == 2 ]]; then variants="after before"; fi
    for variant in $variants; do
        printf 'Round %s: %s\n' "$round" "$variant"
        /usr/bin/time -p "$output/$variant.test" -test.run="$filter" -test.v -test.timeout=12m \
            > "$output/round$round-$variant.log" 2>&1
    done
done
printf 'Results: %s\n' "$output"

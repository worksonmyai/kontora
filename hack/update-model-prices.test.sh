#!/bin/sh
set -eu
export PYTHONDONTWRITEBYTECODE=1

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GENERATOR="$ROOT/hack/update-model-prices.py"
FIXTURES="$ROOT/hack/testdata"
TMP=${TMPDIR:-/tmp}/kontora-model-prices-test.$$
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
mkdir -p "$TMP"

fail() {
    echo "update-model-prices.test: $*" >&2
    exit 1
}

output="$TMP/catalog.json"
printf 'old catalog\n' >"$output"
old_inode=$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_ino)' "$output")
python3 "$GENERATOR" "$FIXTURES/openrouter-models.json" "$output"
cmp "$FIXTURES/openrouter-models.expected.json" "$output" || fail "normalized catalog differs from fixture"
new_inode=$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_ino)' "$output")
[ "$old_inode" != "$new_inode" ] || fail "successful refresh did not replace the destination"

cp "$output" "$TMP/first.json"
python3 "$GENERATOR" "$FIXTURES/openrouter-models.json" "$output"
cmp "$TMP/first.json" "$output" || fail "a repeated refresh was not stable"

for fixture in \
    openrouter-models-missing-data.json \
    openrouter-models-empty-data.json \
    openrouter-models-duplicate.json \
    openrouter-models-empty-id.json \
    openrouter-models-invalid-rate.json \
    openrouter-models-invalid-rate-format.json
do
    printf 'destination must survive\n' >"$output"
    cp "$output" "$TMP/before.json"
    if python3 "$GENERATOR" "$FIXTURES/$fixture" "$output" >"$TMP/stdout" 2>"$TMP/stderr"; then
        fail "$fixture unexpectedly succeeded"
    fi
    cmp "$TMP/before.json" "$output" || fail "$fixture changed the destination on failure"
done

# Exercise a failure after the temporary file has been completely written.
# Patching replace avoids permission-based tests that behave differently as root.
python3 - "$GENERATOR" "$TMP" <<'PY'
import importlib.util
import os
from pathlib import Path
import sys

generator, directory = sys.argv[1:]
spec = importlib.util.spec_from_file_location("update_model_prices", generator)
module = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(module)

destination = Path(directory) / "atomic.json"
destination.write_text("original\n", encoding="utf-8")
original_replace = module.os.replace

def fail_replace(source, target):
    raise OSError("injected rename failure")

module.os.replace = fail_replace
try:
    try:
        module.atomic_write(destination, {"models": []})
    except OSError:
        pass
    else:
        raise SystemExit("atomic_write unexpectedly succeeded")
finally:
    module.os.replace = original_replace

if destination.read_text(encoding="utf-8") != "original\n":
    raise SystemExit("rename failure changed the destination")
if list(destination.parent.glob(".atomic.json.*.tmp")):
    raise SystemExit("rename failure left a temporary file")
PY

printf 'update-model-prices.test: PASS\n'

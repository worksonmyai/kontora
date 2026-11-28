#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
repo="$tmp/repo, with \"quotes\""
mkdir -p "$tmp/bin" "$repo/hack" "$repo/internal/web/testdata/node_modules"
cp hack/test-visual.sh hack/visual.Dockerfile "$repo/hack/"
cp internal/web/testdata/package{,-lock}.json "$repo/internal/web/testdata/"

cat >"$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env python3
import csv
import os
from pathlib import Path
import shutil
import sys

command, *args = sys.argv[1:]
fixture = Path(os.environ['DOCKER_TEST'])
(fixture / f'{command}.args').write_text('\n'.join(args) + '\n')
assert os.environ['DOCKER_CONFIG'] == str(fixture / 'docker-config')
assert os.environ['DOCKER_CONTEXT'] == 'fixture-context'
proxies = 'HTTP_PROXY HTTPS_PROXY FTP_PROXY NO_PROXY ALL_PROXY'.split()
flag = '--build-arg' if command == 'build' else '--env'
assert {proxy + '=' for name in proxies for proxy in (name, name.lower())} <= {
    value for key, value in zip(args, args[1:]) if key == flag
}, f'{command} did not override Docker client proxies'

if command == 'build':
    Path(args[args.index('--iidfile') + 1]).write_text('sha256:fixture\n')
    context = Path(args[-1])
    (fixture / 'context').write_text('\n'.join(sorted(str(p.relative_to(context)) for p in context.rglob('*') if p.is_file())) + '\n')
    sys.exit(int(os.environ.get('DOCKER_BUILD_STATUS', '0')))

assert command == 'run'
with (fixture / 'source.tar').open('wb') as source:
    shutil.copyfileobj(sys.stdin.buffer, source)
mounts = {}
for key, value in zip(args, args[1:]):
    if key == '--mount':
        fields = dict(field.split('=', 1) for field in next(csv.reader([value], strict=True)))
        assert fields['type'] == 'bind'
        mounts[fields['target']] = Path(fields['source'])
root = Path.cwd()
assert mounts.pop('/out/visual') == root / 'test-results/visual'
report = mounts.pop('/out/playwright-report')
assert report == root / 'playwright-report'
report.joinpath('evidence').write_text('renderer evidence\n')

if 'KONTORA_VISUAL_MODE=update' in args:
    staging = mounts.pop('/work/test-results/visual-candidates')
    final = root / 'test-results/visual-candidates'
    assert not final.exists(), 'stale candidates still exposed'
    assert staging.parent == final.parent and staging != final
    assert not list(staging.iterdir()), 'staging directory was not empty'
    (fixture / 'staging').write_text(str(staging))
    result = os.environ.get('DOCKER_UPDATE_RESULT', 'success')
    if result != 'empty':
        project = staging / 'linux-x64-chromium'
        project.mkdir()
        project.joinpath('one.png').write_text('candidate one\n')
        if result == 'partial':
            sys.exit(37)
        project.joinpath('two.png').write_text('candidate two\n')
assert not mounts, f'unexpected mounts: {mounts}'
DOCKER
chmod +x "$tmp/bin/docker"
export PATH="$tmp/bin:$PATH" DOCKER_TEST="$tmp"
export DOCKER_CONTEXT=fixture-context DOCKER_CONFIG="$tmp/docker-config"
export CI='' GITHUB_ACTIONS=''
export KONTORA_CONFIG=/secrets/config.yaml KONTORA_TICKETS_DIR=/secrets/tickets OPENAI_API_KEY=not-a-real-key
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

cd "$repo"
mkdir -p vendor internal/web/{static,shadow} "$tmp/private"
printf 'module wrapper-fixture\ngo 1.27.4\n' >go.mod
printf 'fixture\n' >vendor/modules.txt
printf 'old\n' >internal/web/testdata/mobile_browser.test.mjs
printf 'deleted\n' >internal/web/static/deleted.css
printf 'tracked\n' >internal/web/shadow/asset
printf 'host-only\n' >"$tmp/private/asset"
git init -q
git add -- go.mod vendor/modules.txt internal/web/{shadow/asset,static/deleted.css,testdata/{package.json,package-lock.json,mobile_browser.test.mjs}}
printf 'edited\n' >internal/web/testdata/mobile_browser.test.mjs
rm -- internal/web/{shadow/asset,static/deleted.css}
rmdir internal/web/shadow
ln -s "$tmp/private" internal/web/shadow
ln -s "$tmp/private/asset" internal/web/static/linked.css
printf 'node_modules/\n*.ignored\n' >.gitignore
printf 'host-only\n' >internal/web/static/local.ignored
printf 'host-only\n' >internal/web/testdata/node_modules/sentinel
printf 'host-only\n' >internal/web/testdata/.npmrc
printf 'host-only\n' >internal/web/testdata/.env.local
printf 'host-only\n' >go.work
printf 'new stylesheet\n' >internal/web/static/-new.css
printf 'new unusual asset\n' >$'internal/web/static/with\nnewline.css'
printf 'new visual test\n' >internal/web/testdata/visual.spec.mjs
printf 'new Go test\n' >internal/web/visual_test.go

fail() { echo "FAIL: $*" >&2; exit 1; }
for flag in CI=0 GITHUB_ACTIONS=1; do
    if env "$flag" ./hack/test-visual.sh update >"$tmp/guard" 2>&1; then
        cat "$tmp/guard" >&2
        fail "update accepted $flag"
    fi
    if ! grep -Fq 'disabled in CI' "$tmp/guard"; then
        cat "$tmp/guard" >&2
        fail "missing CI diagnostic for $flag"
    fi
    [[ ! -e $tmp/build.args && ! -e $tmp/run.args ]] || fail "Docker called before rejecting $flag"
done
[[ ! -e test-results && ! -e playwright-report ]] || fail 'CI update created an output directory'

candidates=test-results/visual-candidates
project=$candidates/linux-x64-chromium
seed_stale() {
    mkdir -p "$project"
    printf 'stale candidate\n' >"$project/stale.png"
}
assert_staging_cleaned() {
    [[ ! -e $(<"$tmp/staging") ]] || fail 'staging directory leaked'
    [[ ! -e test-results/.visual-update.lock ]] || fail 'update lock leaked'
}
for mode in check repeat update; do
    if [[ $mode == update ]]; then
        seed_stale
        ./hack/test-visual.sh update
        diff -u <(printf '%s\n' "$project"/{one,two}.png) <(find "$candidates" -type f | LC_ALL=C sort)
        assert_staging_cleaned
    else
        CI=true GITHUB_ACTIONS=true ./hack/test-visual.sh "$mode"
        [[ ! -e $candidates ]] || fail "$mode created the candidate directory"
    fi
    if [[ $mode == check ]]; then
        grep -Fxq CI=1 "$tmp/run.args"
        grep -Fxq GITHUB_ACTIONS=1 "$tmp/run.args"
    elif grep -Eq '^(CI|GITHUB_ACTIONS)=' "$tmp/run.args"; then
        fail "CI env forwarded for $mode"
    fi
    grep -Fxq linux/amd64 "$tmp/build.args"
    for arg in linux/amd64 none KONTORA_TEST_VISUAL=1 "KONTORA_VISUAL_MODE=$mode" sha256:fixture; do
        grep -Fxq -- "$arg" "$tmp/run.args"
    done
    grep -Fq 'timeout=15m' "$tmp/run.args"
    grep -Fq 'if [[ $KONTORA_VISUAL_MODE == repeat ]]; then timeout=25m; fi' "$tmp/run.args"
    grep -Fq 'go test -count=1 -v -timeout "$timeout" ./internal/web -run "^TestVisualBrowserJS$"' "$tmp/run.args"
    if grep -Eq 'KONTORA_CONFIG|KONTORA_TICKETS_DIR|OPENAI_API_KEY|not-a-real-key' "$tmp/run.args"; then
        fail 'host configuration or credentials forwarded'
    fi
done

status=0
DOCKER_UPDATE_RESULT=partial ./hack/test-visual.sh update || status=$?
[[ $status == 37 ]] || fail "failed update returned $status, want 37"
[[ ! -e $candidates ]] || fail 'failed update exposed partial or stale candidates'
[[ $(<playwright-report/evidence) == 'renderer evidence' ]] || fail 'failed update lost its report'
assert_staging_cleaned

seed_stale
if DOCKER_UPDATE_RESULT=empty ./hack/test-visual.sh update >"$tmp/empty" 2>&1; then
    fail 'update accepted empty candidate set'
fi
if ! grep -Fq 'no candidate PNGs' "$tmp/empty"; then
    cat "$tmp/empty" >&2
    fail 'missing empty candidate diagnostic'
fi
[[ ! -e $candidates ]] || fail 'empty update exposed stale candidates'
assert_staging_cleaned

seed_stale
status=0
DOCKER_BUILD_STATUS=38 ./hack/test-visual.sh update || status=$?
[[ $status == 38 ]] || fail "failed build returned $status, want 38"
[[ ! -e $candidates ]] || fail 'failed build exposed stale candidates'
[[ -z $(find test-results -name '.visual-*') ]] || fail 'failed build leaked update state'

diff -u <(printf '%s\n' Dockerfile package-lock.json package.json) "$tmp/context"
python3 - "$tmp/source.tar" <<'PY'
from pathlib import Path
import sys
import tarfile

with tarfile.open(sys.argv[1]) as archive:
    actual = {member.name: archive.extractfile(member).read() for member in archive}
expected = {path: Path(path).read_bytes() for path in (
    'go.mod',
    'internal/web/static/-new.css',
    'internal/web/static/with\nnewline.css',
    'internal/web/testdata/package-lock.json',
    'internal/web/testdata/package.json',
    'internal/web/testdata/visual.spec.mjs',
    'internal/web/visual_test.go',
    'vendor/modules.txt',
)}
expected['internal/web/testdata/mobile_browser.test.mjs'] = b'edited\n'
assert actual == expected, f'source mismatch: {sorted(actual.keys() ^ expected.keys())}'
PY
[[ $(<internal/web/testdata/node_modules/sentinel) == host-only ]] || fail 'host dependencies changed'
echo 'visual wrapper tests passed'

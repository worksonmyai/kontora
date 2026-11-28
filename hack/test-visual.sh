#!/usr/bin/env bash
set -euo pipefail

mode=${1:-check}
if [[ $# -gt 1 || ! $mode =~ ^(check|update|repeat)$ ]]; then
    echo 'Usage: hack/test-visual.sh [check|update|repeat]' >&2
    exit 2
fi
if [[ $mode == update && ( -n ${CI:-} || -n ${GITHUB_ACTIONS:-} ) ]]; then
    echo 'Visual baseline updates are disabled in CI.' >&2
    exit 2
fi

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
checkout=$(pwd -P)

# Only diagnostics (and an empty staging directory for updates) are bind mounted.
outputs=(test-results/visual playwright-report)
for dir in test-results "${outputs[@]}"; do
    if [[ -L $dir ]]; then
        printf 'Refusing symlink at visual output path: %s\n' "$dir" >&2
        exit 1
    fi
    mkdir -p -- "$dir"
done

build='' staging='' update_lock=''
cleanup() {
    if [[ -n $build ]]; then rm -rf -- "$build"; fi
    if [[ -n $staging ]]; then rm -rf -- "$staging"; fi
    if [[ -n $update_lock ]]; then rmdir -- "$update_lock"; fi
}
trap cleanup EXIT
candidates=test-results/visual-candidates
if [[ $mode == update ]]; then
    lock=test-results/.visual-update.lock
    if ! mkdir -- "$lock"; then
        printf 'Cannot lock visual candidate updates: %s\n' "$lock" >&2
        exit 1
    fi
    update_lock=$lock
    if [[ -L $candidates || ( -e $candidates && ! -d $candidates ) ]]; then
        printf 'Visual candidate path is not a regular directory: %s\n' "$candidates" >&2
        exit 1
    fi
    rm -rf -- "$candidates"
    staging=$(mktemp -d "$checkout/test-results/.visual-candidates.XXXXXX")
fi

# Override all defaults injected from Docker client proxy configuration without
# changing the selected context, connection settings, or registry auth.
run=(--rm --init --interactive --platform linux/amd64 --network none --shm-size=1g
    --user "$(id -u):$(id -g)" --env KONTORA_TEST_VISUAL=1 --env "KONTORA_VISUAL_MODE=$mode")
build_args=(--platform linux/amd64)
for proxy in HTTP_PROXY http_proxy HTTPS_PROXY https_proxy FTP_PROXY ftp_proxy NO_PROXY no_proxy ALL_PROXY all_proxy; do
    run+=(--env "$proxy=")
    build_args+=(--build-arg "$proxy=")
done
if [[ $mode == check ]]; then
    if [[ -n ${CI:-} ]]; then run+=(--env CI=1); fi
    if [[ -n ${GITHUB_ACTIONS:-} ]]; then run+=(--env GITHUB_ACTIONS=1); fi
fi

bind_output() {
    # Docker parses --mount as CSV: quote the whole source field, not just its value.
    local source=$1 target=$2
    run+=(--mount "type=bind,\"source=${source//\"/\"\"}\",target=$target")
}
for dir in "${outputs[@]}"; do
    bind_output "$checkout/$dir" "/out/${dir##*/}"
done
if [[ -n $staging ]]; then bind_output "$staging" "/work/$candidates"; fi

regular_checkout_file() {
    local path=$1
    [[ -f $path && ! -L $path ]] || return 1
    # A directory containing tracked files may have been replaced by a symlink.
    while [[ $path == */* ]]; do
        path=${path%/*}
        [[ ! -L $path ]] || return 1
    done
}

# Only the Dockerfile and locked npm manifests belong in the build context.
manifests=(internal/web/testdata/package{,-lock}.json)
for file in hack/visual.Dockerfile "${manifests[@]}"; do
    if ! regular_checkout_file "$file"; then
        printf 'Visual renderer build input is not a regular checkout file: %s\n' "$file" >&2
        exit 1
    fi
done
build=$(mktemp -d)
mkdir "$build/context"
cp hack/visual.Dockerfile "$build/context/Dockerfile"
cp "${manifests[@]}" "$build/context/"
docker build "${build_args[@]}" --iidfile "$build/image" "$build/context"
image=$(<"$build/image")

archive_paths() {
    local file
    while IFS= read -r -d '' file; do
        # Do not send conventional host dependencies, configuration or credentials.
        case /$file in
            */node_modules/*|*/.git/*|*/.docker/*|*/.ssh/*|*/.aws/*|*/.gnupg/*|*/.config/*|*/.kontora/*|*/.npmrc|*/.netrc|*/.env|*/.env.*) continue ;;
        esac
        if regular_checkout_file "$file"; then printf '%s\0' "$file"; fi
    done
}
# Archive the worktree, including unstaged edits and new nonignored source.
git ls-files -z --cached --others --exclude-standard -- go.mod go.sum internal/ vendor/ |
    archive_paths |
    COPYFILE_DISABLE=1 tar --no-recursion --no-xattrs --null -T - -cf - |
    docker run "${run[@]}" "$image" bash -euo pipefail -c '
        HOME=$(mktemp -d)
        export HOME
        tar -xf -
        deps=/opt/kontora-visual
        for manifest in package.json package-lock.json; do
            if ! cmp -s "$deps/$manifest" "internal/web/testdata/$manifest"; then
                echo "Visual renderer: $manifest changed during provisioning; rerun the wrapper." >&2
                exit 1
            fi
        done
        ln -s "$deps/node_modules" internal/web/testdata/node_modules

        go_version=$(go version)
        if [[ $go_version != "go version $KONTORA_VISUAL_GO_VERSION linux/amd64" ]]; then
            echo "Visual renderer: expected $KONTORA_VISUAL_GO_VERSION linux/amd64, got $go_version." >&2
            exit 1
        fi
        (
            cd "$deps"
            node <<"NODE"
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

async function verify() {
    const go = fs.readFileSync("/work/go.mod", "utf8").match(/^go (\S+)$/m)?.[1];
    assert.equal(`go${go}`, process.env.KONTORA_VISUAL_GO_VERSION, "go.mod Go version");
    const version = process.env.KONTORA_VISUAL_PLAYWRIGHT_VERSION;
    assert.equal(require("playwright/package.json").version, version, "Playwright package version");
    assert.equal(require("playwright-core/package.json").version, version, "Playwright Core version");
    assert.equal(process.env.PLAYWRIGHT_BROWSERS_PATH, "/ms-playwright", "browser install path");
    const image = JSON.parse(fs.readFileSync("/ms-playwright/.docker-info", "utf8"));
    assert.equal(image.driverVersion, version, "browser image Playwright version");
    const core = path.dirname(require.resolve("playwright-core/package.json"));
    const { browserVersion } = require(path.join(core, "browsers.json")).browsers.find(
        browser => browser.name === "chromium-headless-shell");
    const browser = await chromium.launch();
    try {
        assert.equal(browser.version(), browserVersion, "Chromium headless version");
    } finally {
        await browser.close();
    }
    console.log(`Verified ${process.env.KONTORA_VISUAL_GO_VERSION}, Playwright ${version}, Chromium ${browserVersion} (linux/amd64)`);
}
verify().catch(error => {
    console.error("Visual renderer verification failed:", error);
    process.exitCode = 1;
});
NODE
        )
        export KONTORA_VISUAL_RENDERER=1
        status=0
        timeout=15m
        if [[ $KONTORA_VISUAL_MODE == repeat ]]; then timeout=25m; fi
        go test -count=1 -v -timeout "$timeout" ./internal/web -run "^TestVisualBrowserJS$" || status=$?

        # Playwright removes and recreates its output directories, so they
        # cannot be bind mount roots. Export local evidence even on failure.
        publish() {
            local source=$1 destination=$2
            find "$destination" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + || return 1
            if [[ -d $source ]]; then cp -R -- "$source/." "$destination/"; fi
        }
        publish_failed=0
        publish test-results/visual /out/visual || publish_failed=1
        publish playwright-report /out/playwright-report || publish_failed=1
        if (( status != 0 )); then exit "$status"; fi
        exit "$publish_failed"
    '

if [[ -n $staging ]]; then
    # The runner must fail if any scene did not capture a stable candidate.
    # Publish the complete set only after both the runner and source pipeline succeed.
    pngs=("$staging"/linux-x64-chromium/*.png)
    if [[ ! -s ${pngs[0]} ]]; then
        echo 'Visual update produced no candidate PNGs.' >&2
        exit 1
    fi
    mv -- "$staging" "$candidates"
    staging=
fi

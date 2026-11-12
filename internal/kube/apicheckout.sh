set -eu
repo="${REPO_DIR:-/work/repo}"; import="${IMPORT_FILE:-/files/import}"
git init -q "$repo"; cd "$repo"
git config core.hooksPath /dev/null
git fast-import --quiet < "$import"
[ "$(git rev-parse refs/heads/base)" = "$BASE_SHA" ] || { echo "the built base commit is not $BASE_SHA" >&2; exit 1; }
[ "$(git rev-parse refs/heads/head)" = "$HEAD_SHA" ] || { echo "the built head commit is not $HEAD_SHA" >&2; exit 1; }
git checkout -q --detach "$HEAD_SHA"

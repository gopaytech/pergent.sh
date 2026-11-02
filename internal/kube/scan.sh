set -eu
# The default branch of one repository, read-only, at one commit. The token
# reaches git only through the auth header (skipped with no token, which only
# a local check uses); no credential helper, no prompt, no config but this
# script's, no hooks, no symlinks (a link in the clone is a plain file), no
# push, https only (redirects too).
export GIT_TERMINAL_PROMPT=0
export GIT_ALLOW_PROTOCOL=https
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export HOME=/work
if [ -n "${GIT_TOKEN:-}" ]; then
  export GIT_CONFIG_COUNT=1
  export GIT_CONFIG_KEY_0=http.extraHeader
  export GIT_CONFIG_VALUE_0="Authorization: Basic $(printf '%s:%s' "${GIT_USERNAME:-}" "$GIT_TOKEN" | base64 -w0)"
fi
unset GIT_TOKEN
fail() {
  printf '%s\n' "$1" > /work/error
  exit 0
}
git init -q /work/repo
cd /work/repo
git config core.hooksPath /dev/null
git config core.symlinks false
git remote add origin -- "$REPO_URL"
git remote set-url --push origin pergent-push-disabled:
if ! git fetch -q --no-tags --depth=1 -- origin "$SCAN_COMMIT"; then
  echo "fetch by SHA failed; falling back to $SCAN_BRANCH" >&2
  if ! git fetch -q --no-tags --depth=1 -- origin "refs/heads/$SCAN_BRANCH"; then
    fail "could not fetch the default branch"
  fi
  head="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
  if [ "$head" != "$SCAN_COMMIT" ]; then
    fail "the default branch moved during the scan; scan again"
  fi
fi
git checkout -q --detach FETCH_HEAD
git rev-parse --verify 'HEAD^{commit}' > /work/commit

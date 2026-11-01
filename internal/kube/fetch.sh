set -eu
# A public repository only: no credential, no credential helper, no prompt,
# no config but this script's, https only (redirects too), no hooks.
export GIT_TERMINAL_PROMPT=0
export GIT_ALLOW_PROTOCOL=https
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export HOME=/work
fail() {
  printf '%s\n' "$1" > /work/error
  exit 0
}
git init -q /work/repo
cd /work/repo
git config core.hooksPath /dev/null
git config core.symlinks true
git remote add origin "$FETCH_URL"
git remote set-url --push origin pergent-push-disabled:
if ! git fetch -q --no-tags --depth=1 origin "$FETCH_REF"; then
  fail "$(printf 'could not fetch %s at %s; the URL, branch or tag may be wrong, or outside jobEgress.cidrs' "$FETCH_URL" "$FETCH_REF")"
fi
if ! git checkout -q --detach FETCH_HEAD; then
  fail "$(printf 'could not check out %s at %s' "$FETCH_URL" "$FETCH_REF")"
fi
git rev-parse --verify 'HEAD^{commit}' > /work/commit

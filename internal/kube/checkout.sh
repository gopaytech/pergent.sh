set -eu
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=http.extraHeader
export GIT_CONFIG_VALUE_0="Authorization: Basic $(printf '%s:%s' "$GIT_USERNAME" "$GIT_TOKEN" | base64 -w0)"
git init -q /work/repo
cd /work/repo
git config core.hooksPath /dev/null
git remote add origin "$REPO_URL"
git remote set-url --push origin pergent-push-disabled:
if ! git fetch -q --no-tags --depth=1 origin "$BASE_SHA" "$HEAD_SHA"; then
  echo "fetch by SHA failed; falling back to $MR_REF and $TARGET_BRANCH" >&2
  git fetch -q --no-tags --depth=1 origin "$MR_REF"
  depth=50
  until git cat-file -e "$BASE_SHA^{commit}" 2>/dev/null; do
    if [ "$depth" -gt 3200 ]; then echo "merge base $BASE_SHA not found within depth 3200" >&2; exit 1; fi
    git fetch -q --no-tags --depth="$depth" origin "$TARGET_BRANCH"
    depth=$((depth * 2))
  done
fi
git checkout -q --detach "$HEAD_SHA"
if [ -n "${LINKED_CLONES:-}" ]; then
  # Linked repositories: one "linked-N <https url>" per line, cloned after the
  # checkout, each into a temporary name and renamed when whole. The token
  # reaches git only through the environment header set above, never in a URL
  # or a file. A clone that fails, runs over its time or is over its size is
  # removed and the Job goes on: pergent reads the missing directory as a
  # failed clone. Nothing here may stop the script.
  printf '%s\n' "$LINKED_CLONES" | while read -r dir url; do
    case "$dir" in linked-[1-3]) ;; *) continue ;; esac
    tmp="/work/.$dir.tmp"
    rm -rf "$tmp" "/work/$dir"
    if (
      export GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=https GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
      timeout 120 git clone -q --depth=1 --no-tags --single-branch -c core.symlinks=false -c core.hooksPath=/dev/null -- "$url" "$tmp" || exit 1
      size="$(du -sk "$tmp")" || exit 1
      size="${size%%[!0-9]*}"
      [ -n "$size" ] && [ "$size" -le 2097152 ] || exit 1
    ) </dev/null && mv "$tmp" "/work/$dir"; then
      echo "cloned $dir" >&2
    else
      rm -rf "$tmp" "/work/$dir"
      echo "could not clone $dir" >&2
    fi
  done || true
fi

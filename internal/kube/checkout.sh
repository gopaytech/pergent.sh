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

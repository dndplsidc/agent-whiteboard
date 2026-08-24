#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

version=$1
semver_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'
if [[ ! $version =~ $semver_pattern ]]; then
  echo "release version must be a semantic version prefixed with v" >&2
  usage
  exit 2
fi

prerelease=${BASH_REMATCH[5]:-}
if [[ -n $prerelease ]]; then
  IFS='.' read -r -a prerelease_identifiers <<< "$prerelease"
  for identifier in "${prerelease_identifiers[@]}"; do
    if [[ $identifier =~ ^[0-9]+$ && $identifier != "0" && $identifier == 0* ]]; then
      echo "numeric prerelease identifiers must not contain leading zeroes" >&2
      exit 2
    fi
  done
fi

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "release must run inside a Git worktree" >&2
  exit 1
}
cd "$root"

if [[ -n $(git status --porcelain --untracked-files=all) ]]; then
  echo "release requires a clean Git worktree" >&2
  exit 1
fi

if git show-ref --verify --quiet "refs/tags/$version"; then
  echo "release tag $version already exists" >&2
  exit 1
fi

git tag --annotate "$version" --message "Agent Whiteboard $version"

cat <<EOF
Created annotated tag $version at $(git rev-parse --short HEAD).
The script does not push commits or tags. Publish explicitly when ready:
  git push origin HEAD
  git push origin refs/tags/$version
EOF

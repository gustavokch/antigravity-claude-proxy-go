#!/usr/bin/env bash
#
# Scan for committed secrets with trufflehog. One entry point for the omp
# extension (.omp/extensions/trufflehog.ts) and for local use; CI runs the
# trufflehog action directly (.github/workflows/secret-scan.yml).
#
#   scripts/secret-scan.sh staged [--all]
#                                    staged content (what `git commit` would record);
#                                    --all adds tracked working-tree edits (`git commit -a`)
#   scripts/secret-scan.sh push      commits not yet on the upstream branch
#   scripts/secret-scan.sh history   every reachable commit
#
# Exit codes: 0 clean, 1 findings, 2 trufflehog missing or usage error.
# Findings are verified or unknown (could not be checked); a secret that
# trufflehog verified as revoked is not reported.
#
# Install: https://github.com/trufflesecurity/trufflehog#installation
# (macOS: brew install trufflehog)

set -uo pipefail

mode="${1:-}"

if ! command -v trufflehog >/dev/null 2>&1; then
  echo "secret-scan: trufflehog not found in PATH." >&2
  echo "secret-scan: install it (macOS: brew install trufflehog) and retry." >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)" || exit 2

common=(--results=verified,unknown --no-update --fail)

scan_staged() {
  local all="${1:-}" work path
  work="$(mktemp -d -t secret-scan)"
  trap 'rm -rf "${work}"' RETURN
  local files=() edited=()
  while IFS= read -r -d '' path; do
    files+=("${path}")
  done < <(git diff --cached --name-only --diff-filter=ACMR -z)
  if [[ "${all}" == "--all" ]]; then
    while IFS= read -r -d '' path; do
      edited+=("${path}")
    done < <(git diff --name-only --diff-filter=ACMR -z)
  fi
  if [[ "${#files[@]}" -eq 0 && "${#edited[@]}" -eq 0 ]]; then
    return 0
  fi
  # checkout-index writes the INDEX blobs, not the working tree: a partially
  # staged file is scanned as it will be committed.
  if [[ "${#files[@]}" -gt 0 ]]; then
    git checkout-index --prefix="${work}/" -- "${files[@]}" || return 2
  fi
  # With --all the commit takes the working-tree version of tracked edits.
  if [[ "${#edited[@]}" -gt 0 ]]; then
    printf '%s\0' "${edited[@]}" | tar --null -T - -cf - | tar -xf - -C "${work}" || return 2
  fi
  trufflehog filesystem "${work}" "${common[@]}" --log-level=-1
}

scan_push() {
  local base
  if base="$(git rev-parse --verify --quiet '@{upstream}')"; then
    :
  elif base="$(git rev-parse --verify --quiet origin/HEAD)"; then
    :
  else
    base=""
  fi
  if [[ -n "${base}" ]]; then
    # --since-commit accepts a commit-ish; use the merge base so a rebased
    # upstream does not widen the range.
    base="$(git merge-base "${base}" HEAD 2>/dev/null || echo "${base}")"
    trufflehog git "file://${PWD}" --since-commit "${base}" --branch HEAD "${common[@]}" --log-level=-1
  else
    trufflehog git "file://${PWD}" --branch HEAD "${common[@]}" --log-level=-1
  fi
}

case "${mode}" in
  staged) scan_staged "${2:-}" ;;
  push) scan_push ;;
  history) trufflehog git "file://${PWD}" "${common[@]}" --log-level=-1 ;;
  *)
    echo "usage: scripts/secret-scan.sh staged [--all]|push|history" >&2
    exit 2
    ;;
esac
rc=$?
if [[ "${rc}" -eq 183 ]]; then
  # trufflehog --fail exit code for "results found".
  rc=1
fi
exit "${rc}"

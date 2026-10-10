#!/usr/bin/env bash
# PreToolUse guard for Edit, Write, MultiEdit and NotebookEdit.
#
# Called with no argument from .claude/settings.json (every session and
# subagent) and with a role name from each subagent's frontmatter.
#
#  1. Role boundaries: a role never edits what it must stay independent of
#     (the plan's build roster: no agent grades its own output).
#  2. Declared files: while /build runs a ticket (tmp/current-task holds its
#     ID), every edit must match a `files:` glob in specs/<ID>.md, which is
#     G1's declared-files rule applied at write time.
#
# Exit 2 blocks the edit and sends the message on stderr back to Claude.
# Edits made through Bash are not seen here; /gates re-checks the diff.
set -euo pipefail
set -f # globs from specs are patterns, never pathnames

role="${1:-}"
input="$(cat)"

if command -v jq >/dev/null 2>&1; then
  file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')"
else
  file="$(printf '%s' "$input" | sed -E -n 's/.*"(file_path|notebook_path)"[[:space:]]*:[[:space:]]*"([^"]*)".*/\2/p' | head -n 1)"
fi
[ -n "$file" ] || exit 0

# The root is the checkout that holds the file: the main checkout or one of
# the ticket worktrees under .worktrees/. Each worktree has its own
# tmp/current-task, so parallel builds each enforce their own spec.
root=""
dir="$(dirname "$file")"
while [ -n "$dir" ] && [ "$dir" != "/" ] && [ ! -d "$dir" ]; do
  dir="$(dirname "$dir")"
done
if [ -d "$dir" ]; then
  root="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null || true)"
fi
[ -n "$root" ] || root="${CLAUDE_PROJECT_DIR:-$(pwd)}"

rel="${file#"$root"/}"

block() {
  printf 'guard-edit: %s\n' "$1" >&2
  exit 2
}

# 1. Role boundaries.
case "$role" in
  domain-data-engineer)
    case "$rel" in
      internal/checks/* | internal/agent/* | internal/evals/*)
        block "domain-data-engineer plants the errors, so it never edits the detectors or the scorer ($rel). Hand this back to the orchestrator." ;;
    esac ;;
  eval-engineer)
    case "$rel" in
      internal/checks/* | internal/agent/* | internal/retrieval/* | evals/baseline.json)
        block "eval-engineer never edits code under test or the baseline ($rel). Hand this back to the orchestrator." ;;
    esac ;;
  llm-engineer)
    case "$rel" in
      internal/evals/* | evals/golden/* | evals/baseline.json | evals/scenarios/*)
        block "llm-engineer never edits the scorer, golden sets, scenarios or baseline ($rel)." ;;
    esac ;;
  implementer)
    case "$rel" in
      internal/seed/* | evals/scenarios/* | evals/baseline.json)
        block "implementer never edits the seeder, scenarios or baseline ($rel); the domain-data-engineer owns them." ;;
    esac ;;
  security-reviewer)
    block "security-reviewer is read-only; report the finding instead of editing $rel." ;;
esac

# 2. Declared files of the ticket /build is running.
task_file="$root/tmp/current-task"
[ -f "$task_file" ] || exit 0

id="$(tr -d '[:space:]' <"$task_file")"
[ -n "$id" ] || exit 0
spec="$root/specs/$id.md"
[ -f "$spec" ] || block "tmp/current-task says $id but specs/$id.md is missing; run /spec $id first."

case "$rel" in
  go.mod | go.sum | "specs/$id.md" | tmp/*) exit 0 ;;
esac

# Read the `files:` list from the YAML front matter (block list form).
globs="$(awk '
  NR == 1 && $0 != "---" { exit }
  NR > 1 && $0 == "---" { exit }
  /^files:/ { in_files = 1; next }
  in_files && /^[[:space:]]*-[[:space:]]/ {
    line = $0
    sub(/^[[:space:]]*-[[:space:]]*/, "", line)
    sub(/[[:space:]]+#.*$/, "", line)
    print line
    next
  }
  in_files && /^[^[:space:]]/ { in_files = 0 }
' "$spec" | tr -d "\"'")"

while IFS= read -r g; do
  [ -n "$g" ] || continue
  g="${g%/}"
  # In [[ == ]] an unquoted pattern's * also matches "/", so ** works too.
  # shellcheck disable=SC2053
  if [[ "$rel" == $g || "$rel" == $g/* ]]; then
    exit 0
  fi
done <<<"$globs"

block "$rel is not in $id's declared files (specs/$id.md). If the ticket really needs it, stop and tell the orchestrator: the spec goes back through G0."

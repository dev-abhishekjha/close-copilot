#!/usr/bin/env bash
# PreToolUse guard for Edit, Write, MultiEdit and NotebookEdit.
#
# Called with no argument from .claude/settings.json (every session and
# subagent) and with a role name from each subagent's frontmatter.
#
#  0. The path is resolved first: relative to the session's cwd, "." and
#     ".." lexically (as Claude Code does before writing), then symlinks.
#     A ".." after a symlink, where the lexical and physical readings
#     differ, is refused as ambiguous. Only paths inside a checkout of this
#     project (the main checkout or a registered worktree under
#     .worktrees/) are judged; anything else is allowed.
#  1. Role boundaries: a role never edits what it must stay independent of
#     (the plan's build roster: no agent grades its own output).
#  2. Declared files: while /build runs a ticket (tmp/current-task in the
#     checkout holding the file names its ID), every edit must match a
#     `files:` glob in specs/<ID>.md as committed at that checkout's HEAD
#     (the working copy only before the spec commit), which is G1's
#     declared-files rule applied at write time, with the gates' glob rules:
#     "*" and "?" stay within one path segment, "**" spans segments, and a
#     trailing "/" means "/**".
#
# Tests: gates/hook_test.go (TestGuardHook*).
#
# Exit 2 blocks the edit and sends the message on stderr back to Claude.
# Claude Code blocks only on exit 2: any other failure, or a timeout, lets
# the edit through. So every unexpected error exits 2 (the ERR trap), and
# input that could take long to judge is refused up front.
# Edits made through Bash are not seen here; /gates re-checks the diff.
set -euo pipefail
set -E
trap 'printf "guard-edit: internal error (line %s)\n" "$LINENO" >&2; exit 2' ERR
set -f # globs from specs are patterns, never pathnames
# Byte semantics: ${#x} counts bytes and [[:print:]] is printable ASCII.
export LC_ALL=C

role="${1:-}"
input="$(cat)"

block() {
  printf 'guard-edit: %s\n' "$1" >&2
  exit 2
}

# Only jq parses JSON escapes correctly; a regex fallback would fail open.
command -v jq >/dev/null 2>&1 || block "jq is required to read the tool input; install jq. Refusing the edit."

# The path is the first non-empty string of file_path and notebook_path
# (jq's // would keep an empty file_path). A non-string path, or one with a
# line break or NUL (which $(...) would strip or drop), is refused: jq
# exits non-zero on error().
# shellcheck disable=SC2016 # $v is jq's variable
if ! file="$(printf '%s' "$input" | jq -r '
  [.tool_input.file_path, .tool_input.notebook_path] as $v
  | if any($v[]; . != null and type != "string") then error("non-string path")
    else ($v | map(select(type == "string" and . != "")) | first // empty)
    end
  | if (explode | any(. == 0 or . == 10 or . == 13)) then error("line break or NUL") else . end
')"; then
  block "could not read the path from the tool input (malformed JSON, a non-string path, or a line break or NUL in the path); refusing the edit."
fi
[ -n "$file" ] || exit 0

# Relative paths resolve against the session's working directory (a worker
# runs in its worktree), as Claude Code resolves them; the project
# directory is the fallback.
if ! cwd="$(printf '%s' "$input" | jq -r 'if (.cwd | type) == "string" then .cwd else "" end')"; then
  block "could not read cwd from the tool input; refusing the edit."
fi
case "$cwd" in
  /*) ;;
  *) cwd="" ;;
esac

# Bound the work before resolving: resolve() forks per segment, and a run
# past the hook's timeout would let the edit through.
if [ "${#file}" -gt 4096 ]; then
  block "the path is longer than 4096 bytes; refusing to edit it."
fi
slashes="${file//[!\/]/}"
if [ "${#slashes}" -gt 256 ]; then
  block "the path has more than 256 segments; refusing to edit it."
fi

# The hook must judge the repository the file is in, not one named by the
# caller's environment.
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_CEILING_DIRECTORIES

# physdir prints the physical path of an existing directory: symlinks
# resolved (macOS /var is /private/var) and, on a case-insensitive disk, the
# case as stored (the external pwd uses getcwd; bash's builtin keeps the
# case it was given).
physdir() {
  (cd -P -- "$1" 2>/dev/null && env pwd -P)
}

# lexical prints the absolute path $1 with "." and empty segments dropped
# and each ".." popping the previous segment, by string alone (no
# filesystem access): what Claude Code writes to.
lexical() {
  local cur="" seg
  local -a segs
  IFS=/ read -r -a segs <<<"$1"
  for seg in ${segs[@]+"${segs[@]}"}; do
    case "$seg" in
      '' | .) ;;
      ..) cur="${cur%/*}" ;;
      *) cur="$cur/$seg" ;;
    esac
  done
  printf '%s\n' "${cur:-/}"
}

# resolve prints the physical path an edit of the absolute path $1 writes
# to. It walks the path one segment at a time: "." is dropped, ".." goes to
# the parent of what has been resolved so far, and each prefix that exists
# as a directory is replaced by its physical path, so ".." after a symlink
# goes where the OS goes. Segments that don't exist yet are taken
# lexically. A final segment that is a symlink is followed (an edit writes
# through it, and the kernel resolves the target physically).
resolve() {
  local path="$1" cur="" seg p hops=0
  local -a segs
  while :; do
    case "$path" in
      /*) ;;
      *) return 1 ;;
    esac
    cur=""
    IFS=/ read -r -a segs <<<"$path"
    for seg in ${segs[@]+"${segs[@]}"}; do
      case "$seg" in
        '' | .) continue ;;
        ..) cur="${cur%/*}" ;;
        *) cur="$cur/$seg" ;;
      esac
      if [ -d "$cur" ] && p="$(physdir "$cur")" && [ -n "$p" ]; then
        cur="$p"
        [ "$cur" != "/" ] || cur=""
      fi
    done
    [ -n "$cur" ] || cur="/"
    if [ -L "$cur" ] && [ ! -d "$cur" ]; then
      hops=$((hops + 1))
      [ "$hops" -le 40 ] || return 1
      p="$(readlink "$cur")" || return 1
      case "$p" in
        /*) path="$p" ;;
        *) path="${cur%/*}/$p" ;;
      esac
      continue
    fi
    printf '%s\n' "$cur"
    return 0
  done
}

case "$file" in
  /*) abs="$file" ;;
  *) abs="${cwd:-${CLAUDE_PROJECT_DIR:-$PWD}}/$file" ;;
esac
lex="$(lexical "$abs")"
resolved="$(resolve "$lex")" || block "could not resolve the path $file (a symlink loop?)."
# The OS reads ".." after a symlink as the target's parent; Claude Code
# drops the previous segment. Where the two disagree, the path is refused
# rather than judged as one and written as the other.
case "/$abs/" in
  */../*)
    physical="$(resolve "$abs")" || block "could not resolve the path $file (a symlink loop?)."
    [ "$physical" = "$resolved" ] ||
      block "the path $file has \"..\" after a symlink, which is ambiguous ($resolved or $physical); use a path without \"..\"." ;;
esac
file="$resolved"

# The root is the checkout that holds the file: the main checkout or one of
# the ticket worktrees under .worktrees/. Each worktree has its own
# tmp/current-task, so parallel builds each enforce their own spec. A path
# outside every checkout of this project (memory files under ~/.claude,
# scratch files, other repositories) is not this hook's business.
dir="$(dirname "$file")"
while [ "$dir" != "/" ] && [ ! -d "$dir" ]; do
  dir="$(dirname "$dir")"
done

common_dir() {
  local d
  d="$(git -C "$1" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 1
  [ -n "$d" ] || return 1
  physdir "$d"
}

project="${CLAUDE_PROJECT_DIR:-$PWD}"
project_common="$(common_dir "$project" || true)"
project_top="$(git -C "$project" rev-parse --show-toplevel 2>/dev/null || true)"
[ -z "$project_top" ] || project_top="$(physdir "$project_top")"

root=""
top="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null || true)"
[ -z "$top" ] || top="$(physdir "$top")"
if [ -n "$top" ]; then
  if [ -z "$project_common" ] || [ "$(common_dir "$dir" || true)" = "$project_common" ]; then
    root="$top"
  fi
fi
# A foreign repository nested inside the project, a bogus .git file that
# makes git fail below it, or a path inside .git (where git finds no work
# tree) is still judged as part of the project: by the checkout of this
# project whose root is the longest prefix of the path, so a worktree's
# files never fall back to the main checkout and lose their role checks.
if [ -z "$root" ] && [ -n "$project_top" ]; then
  best=""
  while IFS= read -r line; do
    case "$line" in
      "worktree "*) ;;
      *) continue ;;
    esac
    w="$(physdir "${line#worktree }" || true)"
    [ -n "$w" ] || continue
    case "$file/" in
      "$w"/*) [ "${#w}" -le "${#best}" ] || best="$w" ;;
    esac
  done < <(git -C "$project_top" worktree list --porcelain 2>/dev/null || true)
  if [ -n "$best" ]; then
    root="$best"
  else
    case "$file/" in
      "$project_top"/*) root="$project_top" ;;
    esac
  fi
fi
[ -n "$root" ] || exit 0

case "$file" in
  "$root"/*) rel="${file#"$root"/}" ;;
  *) exit 0 ;; # the checkout's root directory itself is not a file
esac

# Only printable ASCII is judged (as gates.NonASCIIPaths in G1): APFS folds
# more than ASCII case (evals/baſeline.json is evals/baseline.json), and
# nocasematch below folds only ASCII.
if [[ "$rel" == *[![:print:]]* ]]; then
  block "the path $rel has non-ASCII or control characters; use a printable ASCII path."
fi

# 1. Role boundaries, ignoring case: on a case-insensitive disk (macOS)
# INTERNAL/seed/x.go is internal/seed/x.go.
shopt -s nocasematch
case "$rel" in
  .git | .git/* | */.git | */.git/*) block "edits of or inside a .git are not allowed ($rel)." ;;
esac
# A registered worktree is its own root, so a path still under
# .worktrees/<name>/ here is in a directory git doesn't know as a worktree
# (stale, or made by hand): judged from this checkout, its role patterns
# would never match.
case "$rel" in
  .worktrees/*/*) block "$rel is under .worktrees/ but not in a registered worktree; run git worktree list, and edit only inside a registered worktree." ;;
esac
# The build state that drives the declared-files check is the
# orchestrator's, never a worker's.
if [ -n "$role" ]; then
  case "$rel" in
    tmp/current-task | tmp/erpnext.lock) block "$role never edits $rel; it belongs to the orchestrator." ;;
  esac
fi
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
shopt -u nocasematch

# 2. Declared files of the ticket /build is running.
task_file="$root/tmp/current-task"
if [ ! -e "$task_file" ] && [ ! -L "$task_file" ]; then
  exit 0
fi
[ -f "$task_file" ] || block "tmp/current-task exists but is not a regular file; refusing the edit."

[ -r "$task_file" ] || block "tmp/current-task exists but can't be read; refusing the edit."
if ! id="$(tr -d '[:space:]' <"$task_file")"; then
  block "could not read tmp/current-task; refusing the edit."
fi
[ -n "$id" ] || exit 0
[[ "$id" =~ ^CC-[0-9]+[a-z]?$ ]] || block "tmp/current-task holds $id, which is not a ticket ID."

# The spec as committed binds, so editing the working copy (say, adding
# "**" to files:) widens nothing. Before the spec commit, HEAD lacks it and
# the working copy is all there is.
spec="$root/specs/$id.md"
if spec_text="$(git -C "$root" show "HEAD:specs/$id.md" 2>/dev/null)"; then
  :
elif [ -f "$spec" ]; then
  spec_text="$(cat -- "$spec")" || block "could not read specs/$id.md; refusing the edit."
else
  block "tmp/current-task says $id but specs/$id.md is missing; run /spec $id first."
fi

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
' <<<"$spec_text" | tr -d "\"'")"

# glob_regex prints an anchored ERE for a spec glob under the gates' rules
# (gates/glob.go): "*" and "?" never match "/", "**" as a whole segment
# matches zero or more segments, a trailing "/" is "/**", and every other
# character is literal.
glob_regex() {
  local g="$1" out="" sep=0 i n seg piece ch
  local -a segs
  case "$g" in */) g="$g**" ;; esac
  IFS=/ read -r -a segs <<<"$g"
  n=${#segs[@]}
  for ((i = 0; i < n; i++)); do
    seg="${segs[i]}"
    if [ "$seg" = "**" ]; then
      if [ "$i" -eq $((n - 1)) ]; then
        if [ -z "$out" ]; then out=".*"; else out="$out(/.*)?"; fi
      else
        if [ -z "$out" ]; then out="(.*/)?"; else out="$out/(.*/)?"; fi
      fi
      sep=0
      continue
    fi
    piece=""
    while [ -n "$seg" ]; do
      ch="${seg:0:1}"
      seg="${seg:1}"
      case "$ch" in
        '*') piece="$piece[^/]*" ;;
        '?') piece="$piece[^/]" ;;
        '^' | '\') piece="$piece\\$ch" ;;
        '.' | '[' | ']' | '(' | ')' | '{' | '}' | '+' | '$' | '|') piece="$piece[$ch]" ;;
        *) piece="$piece$ch" ;;
      esac
    done
    if [ "$sep" -eq 1 ]; then out="$out/$piece"; else out="$out$piece"; fi
    sep=1
  done
  printf '^%s$\n' "$out"
}

while IFS= read -r g; do
  [ -n "$g" ] || continue
  re="$(glob_regex "$g")"
  if [[ "$rel" =~ $re ]]; then
    exit 0
  fi
done <<<"$globs"

block "$rel is not in $id's declared files (specs/$id.md). If the ticket really needs it, stop and tell the orchestrator: the spec goes back through G0."

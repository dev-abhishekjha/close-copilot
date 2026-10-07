#!/usr/bin/env bash
# PostToolUse: keep Go files gofmt-clean after every Edit or Write, so lint
# failures are about logic rather than formatting. Never blocks.
input="$(cat)"

if command -v jq >/dev/null 2>&1; then
  file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')"
else
  file="$(printf '%s' "$input" | sed -n 's/.*"file_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
fi

case "$file" in
  *.go)
    if [ -f "$file" ] && command -v gofmt >/dev/null 2>&1; then
      gofmt -w "$file" >&2 || true
    fi ;;
esac
exit 0

#!/usr/bin/env bash
# CC-016a: checks .github/workflows/eval.yml (and, with --with-baseline,
# evals/baseline.json) without actionlint or PyYAML. Run from the repo root.
#
# The "before" files come from the pinned reference 2fef4fe (CC-016's merge:
# the workflow and the version 2 baseline as they were), never from main, so
# the check keeps working after the baseline-update commit merges. It is a
# one-ticket check: delete it when the workflow next changes.
set -euo pipefail

REF=2fef4fe
WF=.github/workflows/eval.yml
BASELINE=evals/baseline.json
with_baseline=false
for arg in "$@"; do
  case "$arg" in
    --with-baseline) with_baseline=true ;;
    *)
      echo "usage: $0 [--with-baseline]" >&2
      exit 2
      ;;
  esac
done

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}
ok() { echo "ok: $*"; }

if [ ! -f "$WF" ]; then
  echo "FAIL: $WF not found; run from the repo root" >&2
  exit 1
fi

work="tmp/check-eval-workflow"
rm -rf "$work"
mkdir -p "$work"
git show "$REF:$WF" >"$work/ref-eval.yml"
git show "$REF:$BASELINE" >"$work/ref-baseline.json"

# (a) one job named "eval (G4)", no matrix; SUITES and LLM_SUITE; no SUITE left.
if ruby -ryaml -e '
  y = YAML.load_file(ARGV[0])
  j = y["jobs"]["eval"] or abort "no jobs.eval"
  abort "job name is #{j["name"].inspect}" unless j["name"] == "eval (G4)"
  abort "job has a strategy" if j.key?("strategy")
  abort "jobs other than eval: #{y["jobs"].keys}" unless y["jobs"].keys == ["eval"]
' "$WF"; then ok "(a) job eval (G4), no strategy"; else fail "(a) job name or strategy"; fi
if grep -q 'SUITES: "suite-skeleton suite-v1"' "$WF"; then ok "(a) SUITES"; else fail "(a) no SUITES: \"suite-skeleton suite-v1\""; fi
if grep -q 'LLM_SUITE: suite-skeleton' "$WF"; then ok "(a) LLM_SUITE"; else fail "(a) no LLM_SUITE: suite-skeleton"; fi
if grep -nE '\$\{?SUITE([^S_A-Z]|$)' "$WF"; then fail "(a) \$SUITE still used (lines above)"; else ok "(a) no \$SUITE"; fi
if grep -nE '^[[:space:]]+SUITE:' "$WF"; then fail "(a) SUITE: env line remains (lines above)"; else ok "(a) no SUITE: env"; fi

# (b) secrets only in tier 2.
if ruby -ryaml -e '
  steps = YAML.load_file(ARGV[0])["jobs"]["eval"]["steps"]
  got = steps.select { |s| s.to_s.include?("secrets.") }.map { |s| s["name"] }
  want = ["tier 2: replay with the model, three runs, and noise"]
  abort "steps using secrets: #{got.inspect}" unless got == want
' "$WF"; then ok "(b) secrets only in tier 2"; else fail "(b) secrets outside tier 2"; fi

# (c) the scorer comes from the base ref; the baseline from the base tree.
if grep -nE 'go run \./cmd/eval (score|noise)' "$WF"; then fail "(c) scorer run from the PR (lines above)"; else ok "(c) no go run score/noise"; fi
bad_compare="$(grep -n -- '--compare' "$WF" | grep -vF '"$GITHUB_WORKSPACE/base/evals/baseline.json"' || true)"
if [ -n "$bad_compare" ]; then
  echo "$bad_compare" >&2
  fail "(c) --compare not against the base tree's baseline"
elif ! grep -q -- '--compare' "$WF"; then
  fail "(c) no --compare at all"
else
  ok "(c) every --compare uses the base baseline"
fi

# (d) every run: block parses.
ruby -ryaml -e '
  steps = YAML.load_file(ARGV[0])["jobs"]["eval"]["steps"]
  steps.each_with_index do |s, i|
    next unless s["run"]
    File.write(File.join(ARGV[1], format("run-%02d.sh", i)), s["run"])
  end
' "$WF" "$work"
d_ok=true
for f in "$work"/run-*.sh; do
  if ! bash -n "$f"; then
    fail "(d) bash -n failed on $f"
    d_ok=false
  fi
done
$d_ok && ok "(d) every run: block passes bash -n"

# (e) fail-closed test of "baseline present (fail closed)".
step="$work/baseline-present.sh"
if ruby -ryaml -e '
  steps = YAML.load_file(ARGV[0])["jobs"]["eval"]["steps"]
  s = steps.find { |x| x["name"] == "baseline present (fail closed)" } or abort "no baseline present step"
  File.write(ARGV[1], s["run"])
' "$WF" "$step"; then
  # run_case <baseline file> <REPLAY> <LLM> <expect: pass|fail> [<text the output must name>]
  run_case() {
    local src="$1" replay="$2" llm="$3" expect="$4" name="${5:-}"
    local case_dir="$work/case-$replay-$llm-$(basename "$src")"
    mkdir -p "$case_dir/ws/base/evals" "$case_dir/rt"
    cp "$src" "$case_dir/ws/base/evals/baseline.json"
    : >"$case_dir/summary.md"
    local rc=0
    env -i PATH="$PATH" HOME="$HOME" \
      GITHUB_WORKSPACE="$PWD/$case_dir/ws" RUNNER_TEMP="$PWD/$case_dir/rt" \
      GITHUB_STEP_SUMMARY="$PWD/$case_dir/summary.md" \
      SUITES="suite-skeleton suite-v1" LLM_SUITE=suite-skeleton REPLAY="$replay" LLM="$llm" \
      bash -e "$step" >"$case_dir/out.log" 2>&1 || rc=$?
    local label="(e) $(basename "$src") REPLAY=$replay LLM=$llm"
    if [ "$expect" = pass ]; then
      if [ "$rc" = 0 ]; then ok "$label passes"; else
        cat "$case_dir/out.log" >&2
        fail "$label exited $rc, want 0"
      fi
    else
      if [ "$rc" = 0 ]; then
        fail "$label passed, want a failure"
      elif [ -n "$name" ] && ! grep -qF "$name" "$case_dir/out.log"; then
        cat "$case_dir/out.log" >&2
        fail "$label failed but did not name $name"
      else
        ok "$label fails naming $name"
      fi
    fi
  }
  jq '{version: 3, suites: {"suite-skeleton": {replay: .tiers.replay, llm: null},
        "suite-v1": {replay: (.tiers.replay + {suite: "suite-v1"}), llm: null}}}' \
    "$work/ref-baseline.json" >"$work/v3-baseline.json"
  run_case "$work/ref-baseline.json" true false fail "suite-v1/replay"
  run_case "$work/v3-baseline.json" true false pass
  run_case "$work/v3-baseline.json" true true fail "suite-skeleton/llm"
else
  fail "(e) no \"baseline present (fail closed)\" step"
fi

# (f) the parts this ticket must not touch equal the pinned reference.
if ruby -ryaml -e '
  cur = YAML.load_file(ARGV[0]); ref = YAML.load_file(ARGV[1])
  bad = []
  on = ->(y) { y.key?("on") ? y["on"] : y[true] }
  bad << "on:" unless on.(cur) == on.(ref)
  %w[permissions concurrency].each { |k| bad << k unless cur[k] == ref[k] }
  cj = cur["jobs"]["eval"]; rj = ref["jobs"]["eval"]
  %w[services timeout-minutes runs-on].each { |k| bad << k unless cj[k] == rj[k] }
  key = ->(s) { s["name"] || s["uses"] }
  names = [
    "check out the base ref (the scorer and the baseline)",
    "check out the code under test",
    "actions/setup-go@v7",
    "build the scorer from the base ref",
    "gitleaks over the fixtures (pinned, checksum verified)",
  ]
  names.each do |n|
    c = cj["steps"].find { |s| key.(s) == n }
    r = rj["steps"].find { |s| key.(s) == n }
    bad << "step #{n.inspect} (missing in reference)" if r.nil?
    bad << "step #{n.inspect}" unless c == r
  end
  abort "changed: #{bad.join(", ")}" unless bad.empty?
' "$WF" "$work/ref-eval.yml"; then ok "(f) unchanged parts equal $REF"; else fail "(f) unchanged parts differ from $REF"; fi

# --with-baseline: the version 3 baseline with both suites.
if $with_baseline; then
  if jq -e '.version == 3' "$BASELINE" >/dev/null; then ok "baseline version 3"; else fail "baseline is not version 3"; fi
  if jq -e '(.suites | keys) == ["suite-skeleton","suite-v1"]' "$BASELINE" >/dev/null; then
    ok "baseline suites"
  else
    fail "baseline suites are not [suite-skeleton, suite-v1]"
  fi
  if jq -e '.suites["suite-skeleton"].llm == null and .suites["suite-v1"].llm == null' "$BASELINE" >/dev/null; then
    ok "baseline llm sections null"
  else
    fail "baseline llm sections not null"
  fi
  if jq -e '.suites["suite-v1"].replay != null' "$BASELINE" >/dev/null; then
    ok "baseline suite-v1 replay present"
  else
    fail "baseline suite-v1 replay missing"
  fi
  if [ "$(jq -S '.tiers.replay' "$work/ref-baseline.json")" = "$(jq -S '.suites["suite-skeleton"].replay' "$BASELINE")" ]; then
    ok "baseline suite-skeleton replay unchanged from $REF"
  else
    fail "baseline suite-skeleton replay differs from $REF"
  fi
fi

if [ "$fails" -gt 0 ]; then
  echo "check-eval-workflow: $fails check(s) failed" >&2
  exit 1
fi
echo "check-eval-workflow: all checks passed"

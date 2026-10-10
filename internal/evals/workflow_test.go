package evals

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The parts of .github/workflows/eval.yml that TestEvalWorkflow checks.
type evalWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Concurrency struct {
		Group string `yaml:"group"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Name     string            `yaml:"name"`
		If       string            `yaml:"if"`
		Env      map[string]string `yaml:"env"`
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
		Steps []evalStep `yaml:"steps"`
	} `yaml:"jobs"`
}

type evalStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	If               string            `yaml:"if"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
}

const (
	evalBin      = `"$RUNNER_TEMP/evalbase/eval"`
	buildScorer  = `go build -o "$RUNNER_TEMP/evalbase/eval" ./cmd/eval`
	baseBaseline = `--compare "$GITHUB_WORKSPACE/base/evals/baseline.json"`
	notOnPR      = "github.event_name != 'pull_request'"
)

var (
	makeRe   = regexp.MustCompile(`(^|[\s;&|(])make(\s|$)`)
	sha256Re = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
)

// readsSecret reports whether a step can see a repository secret or the
// Anthropic key.
func (s evalStep) readsSecret() bool {
	if strings.Contains(s.Run, "secrets.") || strings.Contains(s.Run, "ANTHROPIC_API_KEY") {
		return true
	}
	for k, v := range s.Env {
		if k == "ANTHROPIC_API_KEY" || strings.Contains(v, "secrets.") {
			return true
		}
	}
	for _, v := range s.With {
		if str, ok := v.(string); ok && strings.Contains(str, "secrets.") {
			return true
		}
	}
	return false
}

func (s evalStep) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Uses
}

// TestEvalWorkflow asserts the eval gate's shape (CC-905): its triggers,
// one always-running job, the scorer built from the base ref and run on
// the PR tree against the base tree's baseline, no secret reachable from
// pull_request, no make, a fail-closed baseline check, the job summary,
// and a pinned, checksum-verified gitleaks.
func TestEvalWorkflow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w evalWorkflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}

	// Triggers.
	var triggers []string
	for k := range w.On {
		triggers = append(triggers, k)
	}
	slices.Sort(triggers)
	if !slices.Equal(triggers, []string{"pull_request", "schedule", "workflow_dispatch"}) {
		t.Errorf("triggers %v, want pull_request, schedule and workflow_dispatch only (never pull_request_target)", triggers)
	}
	var pr struct {
		Types []string `yaml:"types"`
	}
	if n, ok := w.On["pull_request"]; !ok || n.Decode(&pr) != nil || !slices.Equal(pr.Types, []string{"opened", "synchronize", "reopened"}) {
		t.Errorf("pull_request types %v", pr.Types)
	}
	var sched []map[string]string
	if n := w.On["schedule"]; n.Decode(&sched) != nil || len(sched) == 0 || sched[0]["cron"] == "" {
		t.Errorf("schedule %v", sched)
	}
	if strings.Contains(string(raw), "pull_request_target") {
		t.Error("eval.yml mentions pull_request_target")
	}
	if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
		t.Errorf("permissions %v, want contents: read only", w.Permissions)
	}
	if !strings.HasPrefix(w.Concurrency.Group, "eval-") {
		t.Errorf("concurrency group %q is not its own", w.Concurrency.Group)
	}

	// One job, always run.
	if len(w.Jobs) != 1 {
		t.Fatalf("%d jobs, want one", len(w.Jobs))
	}
	job, ok := w.Jobs["eval"]
	if !ok {
		t.Fatal("no job eval")
	}
	if job.Name != "eval (G4)" || job.If != "" {
		t.Errorf("job name %q if %q; want eval (G4) with no job-level if", job.Name, job.If)
	}
	if job.Services["postgres"].Image != "pgvector/pgvector:pg17" {
		t.Errorf("postgres service %+v", job.Services)
	}
	for k, v := range job.Env {
		if strings.Contains(v, "secrets.") || k == "ANTHROPIC_API_KEY" {
			t.Errorf("job env %s reads a secret: every step would see it", k)
		}
	}

	steps := job.Steps
	idx := func(ok func(evalStep) bool) int { return slices.IndexFunc(steps, ok) }

	// Checkouts: base/ and pr/ only, no credentials; Go with no cache.
	base := idx(func(s evalStep) bool {
		return strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["path"] == "base"
	})
	prc := idx(func(s evalStep) bool { return strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["path"] == "pr" })
	if base < 0 || prc < 0 {
		t.Fatalf("checkouts: base %d pr %d", base, prc)
	}
	if ref, _ := steps[base].With["ref"].(string); !strings.Contains(ref, "github.event.pull_request.base.sha") || !strings.Contains(ref, "default_branch") {
		t.Errorf("base ref %q: the PR's base, else the default branch", ref)
	}
	for _, s := range steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			if s.With["persist-credentials"] != false {
				t.Errorf("checkout %v keeps credentials", s.With["path"])
			}
			if p := s.With["path"]; p != "base" && p != "pr" {
				t.Errorf("checkout outside base/ and pr/: %v", p)
			}
		}
		if strings.HasPrefix(s.Uses, "actions/setup-go@") && s.With["cache"] != false {
			t.Errorf("setup-go cache %v; want false", s.With["cache"])
		}
		if makeRe.MatchString(s.Run) {
			t.Errorf("step %q calls make: %s", s.label(), s.Run)
		}
		if strings.Contains(s.Run, "gates/") {
			t.Errorf("step %q mentions gates/", s.label())
		}
		if strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates an expression into its script; pass it through env", s.label())
		}
	}

	// The scorer is built in base/, after verifying modules.
	build := idx(func(s evalStep) bool { return strings.Contains(s.Run, buildScorer) })
	if build < 0 || steps[build].WorkingDirectory != "base" || build < base {
		t.Fatalf("scorer build step %d (base checkout %d) must run in base/", build, base)
	}
	if v := strings.Index(steps[build].Run, "go mod verify"); v < 0 || v > strings.Index(steps[build].Run, buildScorer) {
		t.Errorf("scorer build does not run go mod verify first:\n%s", steps[build].Run)
	}

	// Every use of the base-built scorer runs in pr/ after the build; the
	// code under test runs from the PR checkout.
	for i, s := range steps {
		if i != build && strings.Contains(s.Run, evalBin) && (s.WorkingDirectory != "pr" || i < build) {
			t.Errorf("step %q runs the scorer in %q at %d (build at %d)", s.label(), s.WorkingDirectory, i, build)
		}
		if strings.Contains(s.Run, "go run ./cmd/eval score") || strings.Contains(s.Run, "go run ./cmd/eval noise") {
			t.Errorf("step %q scores with the PR's own scorer", s.label())
		}
		if strings.Contains(s.Run, "./cmd/eval run") && (s.WorkingDirectory != "pr" || !strings.Contains(s.Run, "--replay")) {
			t.Errorf("step %q runs eval run outside pr/ or without --replay", s.label())
		}
		if strings.Contains(s.Run, "--compare") && !strings.Contains(s.Run, baseBaseline) {
			t.Errorf("step %q compares with a baseline outside the base tree", s.label())
		}
	}

	// No secret can be read on pull_request.
	for _, s := range steps {
		if s.readsSecret() && !strings.Contains(s.If, notOnPR) {
			t.Errorf("step %q reads a secret but can run on pull_request (if %q)", s.label(), s.If)
		}
	}

	// Tier 1: replay without the model, then the count-based gate.
	t1 := idx(func(s evalStep) bool { return strings.Contains(s.Run, "run --replay --no-llm") })
	if t1 < 0 || steps[t1].readsSecret() || !strings.Contains(steps[t1].If, "steps.tier.outputs.replay") {
		t.Fatalf("tier 1 replay step %d", t1)
	}
	gate1 := idx(func(s evalStep) bool {
		return strings.Contains(s.Run, baseBaseline) && strings.Contains(s.If, "steps.tier.outputs.replay")
	})
	if gate1 < 0 {
		t.Fatal("no tier 1 gate step")
	}
	g := steps[gate1]
	for _, want := range []string{evalBin + " score", `--require 'unrecorded_bank_charge.recall>=3/3'`, `--require 'clean.false_alarms==0'`} {
		if !strings.Contains(g.Run, want) {
			t.Errorf("tier 1 gate lacks %s", want)
		}
	}
	for _, flag := range []string{"--max-p95-increase-pct", "--max-cost-increase-pct", "--min-latency-delta-ms", "unauthorized_writes"} {
		if strings.Contains(g.Run, flag) {
			t.Errorf("tier 1 gate passes %s", flag)
		}
	}
	if g.readsSecret() {
		t.Error("tier 1 gate reads a secret")
	}

	// Tier 2: never on pull_request; the key from a secret, the budget
	// from a variable; thresholds 25, 15 and 1000.
	t2 := idx(func(s evalStep) bool { return s.Env["LLM_PROVIDER"] == "anthropic" })
	if t2 < 0 {
		t.Fatal("no tier 2 step")
	}
	s2 := steps[t2]
	if !strings.Contains(s2.If, notOnPR) || s2.Env["ANTHROPIC_API_KEY"] != "${{ secrets.ANTHROPIC_API_KEY }}" ||
		s2.Env["LLM_DAILY_BUDGET_USD"] != "${{ vars.LLM_DAILY_BUDGET_USD }}" || strings.Contains(s2.Run, "--no-llm") ||
		!strings.Contains(s2.Run, "for i in 1 2 3") || !strings.Contains(s2.Run, evalBin+" noise") {
		t.Errorf("tier 2 step %+v", s2)
	}
	gate2 := idx(func(s evalStep) bool { return strings.Contains(s.Run, "--max-p95-increase-pct 25") })
	if gate2 < 0 || !strings.Contains(steps[gate2].If, notOnPR) || !strings.Contains(steps[gate2].Run, "--max-cost-increase-pct 15") ||
		!strings.Contains(steps[gate2].Run, "--min-latency-delta-ms 1000") || !strings.Contains(steps[gate2].Run, baseBaseline) {
		t.Errorf("tier 2 gate step %d", gate2)
	}

	// Fail closed on a missing baseline, before either gate.
	fc := idx(func(s evalStep) bool {
		return strings.Contains(s.Run, "base/evals/baseline.json") && strings.Contains(s.Run, "exit 1") && !strings.Contains(s.Run, evalBin)
	})
	if fc < 0 || fc > gate1 || fc > gate2 || strings.Contains(steps[fc].If, "always()") {
		t.Errorf("fail-closed baseline step %d (gates %d, %d)", fc, gate1, gate2)
	} else if !strings.Contains(steps[fc].Run, "baseline-update") || !strings.Contains(steps[fc].Run, ".tiers[$t]") {
		t.Errorf("fail-closed step lacks the bootstrap notice or the tier check:\n%s", steps[fc].Run)
	}
	fx := idx(func(s evalStep) bool {
		return strings.Contains(s.Run, "index.json") && strings.Contains(s.Run, "exit 1")
	})
	if fx < 0 || fx > t1 {
		t.Errorf("fail-closed fixtures step %d (replay %d)", fx, t1)
	}

	// The summary is appended to $GITHUB_STEP_SUMMARY, always.
	sum := idx(func(s evalStep) bool {
		return strings.Contains(s.Run, "summary.md") && strings.Contains(s.Run, `>>"$GITHUB_STEP_SUMMARY"`)
	})
	if sum < 0 || !strings.Contains(steps[sum].If, "always()") || sum != len(steps)-1 {
		t.Errorf("summary step %d (of %d) must be last and if: always()", sum, len(steps))
	}
	tierStep := idx(func(s evalStep) bool { return s.ID == "tier" })
	if tierStep < 0 || !strings.Contains(steps[tierStep].Run, "eval (G4): no eval-relevant paths changed") ||
		!strings.Contains(steps[tierStep].Run, "$GITHUB_STEP_SUMMARY") {
		t.Error("the tier step does not note an irrelevant PR in the job summary")
	}
	for _, p := range []string{"internal/(checks|seed|evidence|books|store|evals)/", "cmd/eval/", "evals/", `go\.mod`, `go\.sum`, `\.github/workflows/eval\.yml`} {
		if !strings.Contains(steps[tierStep].Run, p) {
			t.Errorf("the tier step's path filter lacks %s", p)
		}
	}

	// gitleaks: a pinned release with its sha256 checked, over the
	// fixtures, no third-party action.
	gl := idx(func(s evalStep) bool { return strings.Contains(s.Run, "gitleaks") })
	if gl < 0 {
		t.Fatal("no gitleaks step")
	}
	r := steps[gl].Run
	if !strings.Contains(r, "version=8.") || !sha256Re.MatchString(r) || !strings.Contains(r, "sha256sum --check") ||
		!strings.Contains(r, "evals/fixtures") || strings.Contains(r, "latest") {
		t.Errorf("gitleaks is not pinned with a checksum over the fixtures:\n%s", r)
	}
	for _, s := range steps {
		if strings.Contains(s.Uses, "gitleaks") {
			t.Errorf("gitleaks through a third-party action %s", s.Uses)
		}
	}
	if up := idx(func(s evalStep) bool { return strings.HasPrefix(s.Uses, "actions/upload-artifact@") }); up < 0 ||
		!strings.Contains(steps[up].If, "workflow_dispatch") {
		t.Error("the results and candidate baseline are not uploaded on dispatch")
	}
}

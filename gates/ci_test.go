package gates

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The parts of a GitHub Actions workflow that TestCIWorkflow checks.
type ciWorkflow struct {
	On struct {
		PullRequest *struct {
			Types []string `yaml:"types"`
		} `yaml:"pull_request"`
		PullRequestTarget any `yaml:"pull_request_target"`
	} `yaml:"on"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]ciJob `yaml:"jobs"`
}

type ciJob struct {
	Name  string            `yaml:"name"`
	If    string            `yaml:"if"`
	Env   map[string]string `yaml:"env"`
	Steps []ciStep          `yaml:"steps"`
}

type ciStep struct {
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
	baseSHA      = "${{ github.event.pull_request.base.sha }}"
	headSHA      = "${{ github.event.pull_request.head.sha }}"
	gateBin      = "$RUNNER_TEMP/gates/"
	buildGates   = `go build -o "$RUNNER_TEMP/gates/" ./gates/cmd/...`
	approvedFrom = "${{ steps.approval.outputs.sha }}"
)

var (
	goRunGates = regexp.MustCompile(`go\s+(run|build|install)\b[^\n]*\bgates/`)
	makeCall   = regexp.MustCompile(`(^|[\s;&|(])make(\s|$)`)
	gateCall   = regexp.MustCompile(`"\$RUNNER_TEMP/gates/([a-z]+)"`)
)

func loadWorkflows(t *testing.T) map[string]ciWorkflow {
	t.Helper()
	paths, err := filepath.Glob("../.github/workflows/*.y*ml")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]ciWorkflow)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var w ciWorkflow
		if err := yaml.Unmarshal(data, &w); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[filepath.Base(p)] = w
	}
	if len(out) == 0 {
		t.Fatal("no workflows under .github/workflows")
	}
	return out
}

func stepIndex(steps []ciStep, ok func(ciStep) bool) int {
	return slices.IndexFunc(steps, ok)
}

func withString(s ciStep, key string) string {
	v, _ := s.With[key].(string)
	return v
}

// TestCIWorkflow asserts the shape of .github/workflows: gate binaries are
// built from the base ref, with no restored cache and verified modules, and
// run against the PR tree (finding 1), the protected gate takes an approved
// SHA from the labeled event (finding 2), label and edit events can neither
// cancel nor skip check and integration (finding 5), the gates rerun when
// the PR's base changes, and .github/BRANCH_PROTECTION.md names every job
// as a required check.
func TestCIWorkflow(t *testing.T) {
	wfs := loadWorkflows(t)
	invoked := make(map[string]string) // gate command -> workflow/job

	for file, w := range wfs {
		if w.On.PullRequestTarget != nil {
			t.Errorf("%s: pull_request_target runs with a write token on PR events; use pull_request", file)
		}
		for name, job := range w.Jobs {
			where := file + " job " + name
			if job.If != "" {
				t.Errorf("%s has if %q; a skipped required check counts as passing", where, job.If)
			}
			runsGates := false
			for _, s := range job.Steps {
				if goRunGates.MatchString(s.Run) && s.WorkingDirectory != "base" {
					t.Errorf("%s step %q builds or runs gate code outside the base checkout: %s", where, s.Name, s.Run)
				}
				if makeCall.MatchString(s.Run) {
					t.Errorf("%s step %q calls make, which runs the PR's own gate code: %s", where, s.Name, s.Run)
				}
				if strings.Contains(s.Run, gateBin) {
					runsGates = true
				}
			}
			if runsGates {
				checkBaseRefGates(t, where, job, invoked)
			}
		}
	}

	// Every gate command except ready (G0, run by /build before a worker
	// starts) runs in CI from the base ref.
	cmds, err := os.ReadDir("cmd")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cmds {
		if !c.IsDir() || c.Name() == "ready" {
			continue
		}
		if _, ok := invoked[c.Name()]; !ok {
			t.Errorf("gates/cmd/%s is not run from base-ref binaries in any workflow", c.Name())
		}
	}

	ci, ok := wfs["ci.yml"]
	if !ok {
		t.Fatal("no .github/workflows/ci.yml")
	}
	prot, ok := wfs["protected.yml"]
	if !ok {
		t.Fatal("no .github/workflows/protected.yml")
	}
	gw, ok := wfs["gates.yml"]
	if !ok {
		t.Fatal("no .github/workflows/gates.yml")
	}

	t.Run("label events never cancel or skip check and integration", func(t *testing.T) {
		if ci.On.PullRequest == nil {
			t.Fatal("ci.yml does not run on pull_request")
		}
		if got, want := ci.On.PullRequest.Types, []string{"opened", "synchronize", "reopened"}; !slices.Equal(got, want) {
			t.Errorf("ci.yml pull_request types = %v, want %v", got, want)
		}
		for _, name := range []string{"check", "integration"} {
			job, ok := ci.Jobs[name]
			if !ok {
				t.Errorf("ci.yml has no %s job", name)
				continue
			}
			if job.If != "" {
				t.Errorf("ci.yml job %s has if %q; it must run on every pull_request and push", name, job.If)
			}
		}
		for name, job := range ci.Jobs {
			if strings.Contains(job.If, "label") {
				t.Errorf("ci.yml job %s depends on label events: %q", name, job.If)
			}
		}
	})

	t.Run("protected runs on label events in its own concurrency group", func(t *testing.T) {
		if prot.On.PullRequest == nil {
			t.Fatal("protected.yml does not run on pull_request")
		}
		for _, typ := range []string{"opened", "synchronize", "reopened", "labeled", "unlabeled", "edited"} {
			if !slices.Contains(prot.On.PullRequest.Types, typ) {
				t.Errorf("protected.yml does not run on %s", typ)
			}
		}
		if prot.Concurrency.Group == "" || prot.Concurrency.Group == ci.Concurrency.Group || strings.HasPrefix(prot.Concurrency.Group, "ci-") {
			t.Errorf("protected.yml concurrency group %q must differ from ci.yml's %q", prot.Concurrency.Group, ci.Concurrency.Group)
		}
		// No other workflow that runs on label or edit events shares
		// ci.yml's group, where it would cancel check and integration.
		for file, w := range wfs {
			if file == "ci.yml" || w.On.PullRequest == nil {
				continue
			}
			if slices.ContainsFunc(w.On.PullRequest.Types, func(typ string) bool {
				return typ == "labeled" || typ == "unlabeled" || typ == "edited"
			}) && (w.Concurrency.Group == ci.Concurrency.Group || strings.HasPrefix(w.Concurrency.Group, "ci-")) {
				t.Errorf("%s runs on label or edit events in ci.yml's concurrency group", file)
			}
		}
		job, ok := prot.Jobs["protected"]
		if !ok {
			t.Fatal("protected.yml has no protected job")
		}
		if job.If != "" {
			t.Errorf("protected job has if %q; a skipped run would count as green", job.If)
		}
		if invoked["protected"] != "protected.yml job protected" {
			t.Errorf("gates/cmd/protected runs in %q, want protected.yml job protected", invoked["protected"])
		}
	})

	t.Run("approval is pinned to the labeled head SHA", func(t *testing.T) {
		job := prot.Jobs["protected"]
		ai := stepIndex(job.Steps, func(s ciStep) bool { return s.ID == "approval" })
		if ai < 0 {
			t.Fatal("protected job has no step with id approval")
		}
		a := job.Steps[ai]
		if a.Env["HEAD_SHA"] != headSHA || a.Env["LABEL"] != "${{ github.event.label.name }}" || a.Env["ACTION"] != "${{ github.event.action }}" {
			t.Errorf("approval step env = %v; want HEAD_SHA, LABEL and ACTION from the event payload", a.Env)
		}
		for _, want := range []string{`"$ACTION" = "labeled"`, `"$LABEL" = "approved"`, `sha="$HEAD_SHA"`, `"$GITHUB_OUTPUT"`} {
			if !strings.Contains(a.Run, want) {
				t.Errorf("approval step run lacks %s:\n%s", want, a.Run)
			}
		}
		if strings.Contains(a.Run, "${{") {
			t.Errorf("approval step interpolates expressions into the script; pass them through env:\n%s", a.Run)
		}
		pi := stepIndex(job.Steps, func(s ciStep) bool { return strings.Contains(s.Run, `"`+gateBin+`protected"`) })
		if pi < 0 {
			t.Fatal("protected job does not run the protected gate")
		}
		p := job.Steps[pi]
		if pi < ai || !strings.Contains(p.Run, `--approved-sha "$APPROVED_SHA"`) || p.Env["APPROVED_SHA"] != approvedFrom {
			t.Errorf("protected step must follow the approval step and pass --approved-sha \"$APPROVED_SHA\" with APPROVED_SHA=%s: %+v", approvedFrom, p)
		}
		if strings.Contains(p.Run, "--labels") {
			t.Errorf("protected step passes --labels; labels come from the event payload: %s", p.Run)
		}
	})

	t.Run("gates job runs graph, declared and lint, and reruns on a base change", func(t *testing.T) {
		for _, c := range []string{"graph", "declared", "lint"} {
			if invoked[c] != "gates.yml job gates" {
				t.Errorf("gates/cmd/%s runs in %q, want gates.yml job gates", c, invoked[c])
			}
		}
		if gw.On.PullRequest == nil {
			t.Fatal("gates.yml does not run on pull_request")
		}
		// edited is how GitHub reports a changed base branch.
		for _, typ := range []string{"opened", "synchronize", "reopened", "edited"} {
			if !slices.Contains(gw.On.PullRequest.Types, typ) {
				t.Errorf("gates.yml does not run on %s", typ)
			}
		}
		if g := gw.Concurrency.Group; g == "" || g == ci.Concurrency.Group || g == prot.Concurrency.Group {
			t.Errorf("gates.yml concurrency group %q must differ from ci.yml's %q and protected.yml's %q", g, ci.Concurrency.Group, prot.Concurrency.Group)
		}
	})

	t.Run("BRANCH_PROTECTION.md requires every job", func(t *testing.T) {
		data, err := os.ReadFile("../.github/BRANCH_PROTECTION.md")
		if err != nil {
			t.Fatal(err)
		}
		doc := string(data)
		for file, w := range wfs {
			for name, job := range w.Jobs {
				if job.Name == "" {
					t.Errorf("%s job %s has no name; required checks are named by it", file, name)
					continue
				}
				if !strings.Contains(doc, "`"+job.Name+"`") {
					t.Errorf("BRANCH_PROTECTION.md does not list %s job %s (%q) as a required check", file, name, job.Name)
				}
			}
		}
		for _, want := range []string{"Code Owners", "Dismiss stale pull request approvals", "Re-running a workflow run replays", "Bootstrapping this change", "--approved-sha"} {
			if !strings.Contains(doc, want) {
				t.Errorf("BRANCH_PROTECTION.md lacks %q", want)
			}
		}
	})
}

// checkBaseRefGates asserts that a job building or running gate binaries
// checks the base ref out into base/, builds every gates/cmd there, and runs
// the binaries only inside the PR checkout pr/, after the build.
func checkBaseRefGates(t *testing.T, where string, job ciJob, invoked map[string]string) {
	t.Helper()
	isCheckout := func(s ciStep) bool { return strings.HasPrefix(s.Uses, "actions/checkout@") }
	bi := stepIndex(job.Steps, func(s ciStep) bool {
		return isCheckout(s) && withString(s, "path") == "base"
	})
	if bi < 0 {
		t.Errorf("%s runs gate binaries without checking the base ref out into base/", where)
		return
	}
	b := job.Steps[bi]
	if withString(b, "ref") != baseSHA {
		t.Errorf("%s base checkout ref = %q, want %s", where, withString(b, "ref"), baseSHA)
	}
	pri := stepIndex(job.Steps, func(s ciStep) bool {
		return isCheckout(s) && withString(s, "path") == "pr"
	})
	if pri < 0 {
		t.Errorf("%s has no PR checkout in pr/ (a checkout at the workspace root would hold base/ as untracked files)", where)
	} else if withString(job.Steps[pri], "ref") != headSHA {
		t.Errorf("%s PR checkout ref = %q, want %s", where, withString(job.Steps[pri], "ref"), headSHA)
	}
	for _, i := range []int{bi, pri} {
		if i >= 0 && job.Steps[i].With["persist-credentials"] != false {
			t.Errorf("%s checkout %q keeps credentials; set persist-credentials: false", where, withString(job.Steps[i], "path"))
		}
	}
	for _, s := range job.Steps {
		if isCheckout(s) && withString(s, "path") != "base" && withString(s, "path") != "pr" {
			t.Errorf("%s has a checkout outside base/ and pr/: %v", where, s.With)
		}
	}

	ui := stepIndex(job.Steps, func(s ciStep) bool { return strings.Contains(s.Run, buildGates) })
	if ui < 0 {
		t.Errorf("%s does not build the gates with %s", where, buildGates)
		return
	}
	if job.Steps[ui].WorkingDirectory != "base" || ui < bi {
		t.Errorf("%s builds the gates in %q at step %d, want base/ after the base checkout (step %d)", where, job.Steps[ui].WorkingDirectory, ui, bi)
	}
	// The base build verifies its modules and restores no cache: a cache
	// saved by the PR's own jobs could carry planted objects.
	build := job.Steps[ui].Run
	if v := strings.Index(build, "go mod verify"); v < 0 || v > strings.Index(build, buildGates) {
		t.Errorf("%s does not run go mod verify in base/ before building the gates:\n%s", where, build)
	}
	setups := 0
	for i, s := range job.Steps {
		if !strings.HasPrefix(s.Uses, "actions/setup-go@") {
			continue
		}
		setups++
		if i > ui {
			t.Errorf("%s sets up Go after building the gates", where)
		}
		if s.With["cache"] != false {
			t.Errorf("%s setup-go has cache %v; the base-ref build needs cache: false", where, s.With["cache"])
		}
		if withString(s, "go-version-file") != "base/go.mod" {
			t.Errorf("%s setup-go reads %q; the gate build's Go comes from base/go.mod", where, withString(s, "go-version-file"))
		}
	}
	if setups == 0 {
		t.Errorf("%s builds the gates without actions/setup-go (cache: false)", where)
	}
	for i, s := range job.Steps {
		if i == ui || !strings.Contains(s.Run, gateBin) {
			continue
		}
		if s.WorkingDirectory != "pr" || i < ui {
			t.Errorf("%s step %q runs gate binaries in %q at step %d, want pr/ after the build (step %d)", where, s.Name, s.WorkingDirectory, i, ui)
		}
		for _, m := range gateCall.FindAllStringSubmatch(s.Run, -1) {
			invoked[m[1]] = where
		}
	}
}

//go:build integration

package agent

// Gate B (CC-703): the close workflow against Postgres (testcontainers) and
// the real books and evidence MCP servers over the fake ERPNext's skeleton
// month. internal/agent may not link the ERPNext client, even in tests
// (noerpimport), so the servers run in the fakeerpd helper binary and this
// test receives only their URLs and a token.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// The skeleton month the helper serves.
const (
	skeletonCompany = "sharma"
	skeletonMonth   = "2026-09"
)

// testGo returns the go command of the toolchain running this test.
func testGo(t *testing.T) string {
	t.Helper()
	//nolint:staticcheck // SA1019: a test binary runs on the machine that built it, so its GOROOT is the toolchain to ask.
	if root := runtime.GOROOT(); root != "" {
		p := filepath.Join(root, "bin", "go")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command: %v", err)
	}
	return p
}

// fakeServers are the MCP endpoints fakeerpd serves.
type fakeServers struct {
	BooksURL    string `json:"books_url"`
	EvidenceURL string `json:"evidence_url"`
	Token       string `json:"token"`
}

// startFakeERP builds and starts fakeerpd over databaseURL, which seeds the
// skeleton month's bank lines, and returns its endpoints.
func startFakeERP(t *testing.T, databaseURL string) fakeServers {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeerpd")
	build := exec.CommandContext(t.Context(), testGo(t), "build", "-o", bin, "../testsupport/fakeerp/cmd/fakeerpd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fakeerpd: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "-statement", "skeleton")
	cmd.Env = []string{"DATABASE_URL=" + databaseURL, "PATH=" + os.Getenv("PATH")}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("fakeerpd did not start: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	var s fakeServers
	if err := json.Unmarshal([]byte(line), &s); err != nil || s.BooksURL == "" || s.EvidenceURL == "" {
		t.Fatalf("fakeerpd ready line %q: %v", line, err)
	}
	return s
}

// databaseURL returns the connection string of a running store's pool.
func databaseURL(st *store.Store) string {
	return st.Pool().Config().ConnString()
}

type gateB struct {
	st       *store.Store
	reg      *Registry
	profiles map[string]company.Profile
	rules    company.Rules
}

func setupGateB(t *testing.T) *gateB {
	t.Helper()
	st := setupStore(t)
	srv := startFakeERP(t, databaseURL(st))
	reg, err := NewRegistry(t.Context(), config.Config{
		BooksMCPURL: srv.BooksURL, EvidenceMCPURL: srv.EvidenceURL, MCPTokenAgent: config.NewSecret(srv.Token),
	}, discardLog())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(reg.Close)
	profiles, err := company.LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := company.LoadRules(filepath.Join("..", "..", "config", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]company.Profile{}
	for _, p := range profiles {
		byID[p.ID] = p
	}
	return &gateB{st: st, reg: reg, profiles: byID, rules: rules}
}

// workflow is a Gate B workflow with the fake explainer, verifier and
// model; tests take them away as needed.
func (g *gateB) workflow(t *testing.T) (*Workflow, *fakeExplainer) {
	t.Helper()
	expl := &fakeExplainer{st: g.st}
	return &Workflow{
		Store:      g.st,
		Books:      &MCPBooks{Registry: g.reg, Companies: g.st},
		Evidence:   &MCPEvidence{Registry: g.reg},
		Profiles:   g.profiles,
		Rules:      g.rules,
		Explainer:  expl,
		Verifier:   &fakeVerifier{st: g.st},
		Model:      fakeModel(),
		Config:     config.Config{LLMDailyBudgetUSD: 2, LLMRunTokenCap: 200000},
		ResultsDir: t.TempDir(),
		Log:        discardLog(),
	}, expl
}

// stepStatuses is kind -> sorted statuses of a stored run.
func stepStatuses(t *testing.T, st *store.Store, runID uuid.UUID) map[string][]string {
	t.Helper()
	steps, err := st.ListSteps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, s := range steps {
		key := s.Kind + "|" + s.Subject
		if seen[key] {
			t.Errorf("duplicate step %s", key)
		}
		seen[key] = true
		out[s.Kind] = append(out[s.Kind], s.Status)
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func TestWorkflowGateB(t *testing.T) {
	g := setupGateB(t)
	ctx := t.Context()

	t.Run("skeleton month ends done with fakes", func(t *testing.T) {
		wf, _ := g.workflow(t)
		res, err := wf.RunClose(ctx, skeletonCompany, skeletonMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunDone || res.Findings != 3 {
			t.Fatalf("result %+v, want done with 3 findings", res)
		}
		wantSteps(t, stepStatuses(t, g.st, res.RunID), map[string][]string{
			"check.bankrec": {"done"},
			"explain":       repeat("done", 3),
			"investigate":   {"skipped"},
			"retrieve":      {"skipped"},
			"router":        {"done"},
			"synthesize":    {"done"},
			"verify":        repeat("done", 3),
		})
		fs, err := g.st.ListFindingsByRun(ctx, res.RunID)
		if err != nil {
			t.Fatal(err)
		}
		charges := map[string]bool{}
		for _, f := range fs {
			if f.Type != checks.TypeUnrecordedBankCharge {
				t.Errorf("unexpected finding %s: %s", f.Type, f.Title)
			}
			charges[f.Keys["bank_txn_id"]] = true
			for _, e := range f.Evidence {
				a, err := g.st.GetArtifact(ctx, e.Artifact)
				if err != nil || a.Kind != store.ArtifactToolResult {
					t.Errorf("finding %s evidence %s: %v", f.ID, e.Artifact, err)
				}
			}
		}
		for _, txn := range []string{"HDFC-20260915-C1", "HDFC-20260920-C2", "HDFC-20260930-C3"} {
			if !charges[txn] {
				t.Errorf("planted charge %s not found (found %v)", txn, charges)
			}
		}

		// The report file and its artifact.
		md, err := os.ReadFile(res.ReportPath)
		if err != nil {
			t.Fatal(err)
		}
		steps, _ := g.st.ListSteps(ctx, res.RunID)
		var reportSHA string
		for _, s := range steps {
			if s.Kind == store.StepKindSynthesize {
				reportSHA = s.OutputRefs[0]
			}
		}
		a, err := g.st.GetArtifact(ctx, reportSHA)
		if err != nil || a.Kind != store.ArtifactReport {
			t.Fatalf("report artifact: %v %+v", err, a.Kind)
		}
		var art ReportArtifact
		if err := json.Unmarshal(a.Content, &art); err != nil || art.Markdown != string(md) {
			t.Errorf("report artifact differs from the file: %v", err)
		}
		for _, want := range []string{"| Outcome | done |", "₹5.90", "₹590.00", "₹17.70", "HDFC-20260920-C2"} {
			if !strings.Contains(string(md), want) {
				t.Errorf("report lacks %q", want)
			}
		}

		// Usage rolled up into close_runs from the recorded calls.
		run, err := g.st.GetCloseRun(ctx, res.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != store.RunDone || run.InputTokens != 3*80 || run.OutputTokens != 3*20 || run.FinishedAt == nil {
			t.Errorf("close run %s tokens %d/%d finished %v", run.Status, run.InputTokens, run.OutputTokens, run.FinishedAt)
		}
	})

	t.Run("no explainer ends partial", func(t *testing.T) {
		wf, _ := g.workflow(t)
		wf.Explainer, wf.Verifier, wf.Model = nil, nil, nil
		res, err := wf.RunClose(ctx, skeletonCompany, skeletonMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunPartial || res.Findings != 3 {
			t.Fatalf("result %+v, want partial", res)
		}
		wantSteps(t, stepStatuses(t, g.st, res.RunID), map[string][]string{
			"check.bankrec": {"done"},
			"explain":       repeat("skipped", 3),
			"investigate":   {"skipped"},
			"retrieve":      {"skipped"},
			"router":        {"done"},
			"synthesize":    {"done"},
			"verify":        repeat("skipped", 3),
		})
		if _, err := os.Stat(res.ReportPath); err != nil {
			t.Errorf("report file: %v", err)
		}

		// Resuming with an explainer finishes it, with no duplicate step
		// and the same findings.
		before, _ := g.st.ListFindingsByRun(ctx, res.RunID)
		wf2, _ := g.workflow(t)
		res2, err := wf2.ResumeClose(ctx, res.RunID)
		if err != nil || res2.Status != store.RunDone {
			t.Fatalf("resume %+v: %v", res2, err)
		}
		after, _ := g.st.ListFindingsByRun(ctx, res.RunID)
		if normalized(t, before) != normalized(t, after) || len(after) != 3 {
			t.Error("findings changed across the resume")
		}
		stepStatuses(t, g.st, res.RunID) // checks for duplicates
	})

	t.Run("month without evidence is refused", func(t *testing.T) {
		wf, expl := g.workflow(t)
		res, err := wf.RunClose(ctx, skeletonCompany, "2026-10")
		if err == nil || res.Status != store.RunFailed || !strings.Contains(res.Reason, "evidence for sharma 2026-10 is not loaded") {
			t.Fatalf("result %+v, err %v; want a preflight refusal", res, err)
		}
		wantSteps(t, stepStatuses(t, g.st, res.RunID), map[string][]string{"router": {"failed"}})
		if expl.calls != 0 {
			t.Error("refused run explained findings")
		}
	})

	t.Run("token cap ends partial", func(t *testing.T) {
		wf, _ := g.workflow(t)
		wf.Parallel = 1
		wf.Config.LLMRunTokenCap = 150 // each call uses 100
		res, err := wf.RunClose(ctx, skeletonCompany, skeletonMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunPartial || !strings.Contains(res.Reason, "explanations missing for 1 of 3 findings") {
			t.Fatalf("result %+v", res)
		}
		if n, _ := g.st.RunTokensUsed(ctx, res.RunID); n != 200 {
			t.Errorf("tokens used %d, want 200", n)
		}
	})

	// Last: it spends today's budget for every later run in this database.
	t.Run("exhausted budget is refused", func(t *testing.T) {
		spend := createRun(t, g.st)
		step, _, err := g.st.Begin(ctx, spend, store.StepKindExplain, "spend")
		if err != nil {
			t.Fatal(err)
		}
		p, _ := g.st.PutArtifact(ctx, store.ArtifactPrompt, spend, step.ID, "synthetic prompt")
		r, _ := g.st.PutArtifact(ctx, store.ArtifactResponse, spend, step.ID, "synthetic response")
		if _, err := g.st.InsertLLMCall(ctx, store.LLMCall{RunID: spend, StepID: step.ID, Model: "claude-haiku-5-5",
			PromptSHA256: p, ResponseSHA256: r, InputTokens: 1, OutputTokens: 1, CostUSD: "2"}); err != nil {
			t.Fatal(err)
		}
		wf, expl := g.workflow(t)
		res, err := wf.RunClose(ctx, skeletonCompany, skeletonMonth)
		if err == nil || res.Status != store.RunFailed || !strings.Contains(res.Reason, "daily LLM budget") {
			t.Fatalf("result %+v, err %v; want a budget refusal", res, err)
		}
		wantSteps(t, stepStatuses(t, g.st, res.RunID), map[string][]string{"router": {"failed"}})
		if expl.calls != 0 {
			t.Error("refused run explained findings")
		}
	})
}

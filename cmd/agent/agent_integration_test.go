//go:build integration

package main

// Gate B (CC-703) end to end: the agent binary, built here, runs a close
// against Postgres (testcontainers) and the real books and evidence MCP
// servers over the fake ERPNext's skeleton month, served in this test
// process. The binary has no explainer until CC-704, so a run ends
// partial with its explain and verify steps skipped; that is the expected
// end state of these tests.
//
// To stop a run at a known point, a test installs a trigger that makes the
// insert of the retrieve step sleep in Postgres: by then preflight and the
// bank reconciliation check are done, and the run is blocked mid-way.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

type cliEnv struct {
	st      *store.Store
	dbURL   string
	stack   *fakeerp.Stack
	bin     string
	results string
}

func setupCLI(t *testing.T) *cliEnv {
	t.Helper()
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
		postgres.WithDatabase("test_copilot"),
		postgres.WithUsername("copilot"),
		postgres.WithPassword("copilot"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start testcontainers postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(pg); err != nil {
			t.Logf("terminate testcontainers postgres: %v", err)
		}
	})
	dbURL, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	lines, err := fakeerp.SkeletonStatement()
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeerp.Seed(ctx, st, lines); err != nil {
		t.Fatal(err)
	}
	stack, err := fakeerp.Start(st, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stack.Close)

	bin := filepath.Join(t.TempDir(), "agent")
	if out, err := exec.CommandContext(t.Context(), goTool(t), "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return &cliEnv{st: st, dbURL: dbURL, stack: stack, bin: bin, results: t.TempDir()}
}

// command builds an agent command with only the environment it needs.
func (e *cliEnv) command(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	configDir, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, "--results-dir", e.results, "--config-dir", configDir)
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"DATABASE_URL=" + e.dbURL,
		"BOOKS_MCP_URL=" + e.stack.BooksURL,
		"EVIDENCE_MCP_URL=" + e.stack.EvidenceURL,
		"MCP_TOKEN_AGENT=" + e.stack.Token,
		"LLM_PROVIDER=claude-cli",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return cmd, &stdout, &stderr
}

func (e *cliEnv) runCmd(t *testing.T, args ...string) (resultLine, error) {
	t.Helper()
	cmd, stdout, stderr := e.command(t, args...)
	err := cmd.Run()
	if err != nil {
		t.Logf("agent %v: %v\nstderr: %s", args, err, stderr)
	}
	var res resultLine
	if line := strings.TrimSpace(stdout.String()); line != "" {
		if jerr := json.Unmarshal([]byte(line), &res); jerr != nil {
			t.Fatalf("result line %q: %v", line, jerr)
		}
	}
	return res, err
}

// holdAtRetrieve makes the insert of a retrieve step sleep in Postgres;
// release drops the trigger and ends any sleeping backend.
func (e *cliEnv) holdAtRetrieve(t *testing.T) (release func()) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION cc703_hold() RETURNS trigger AS $$
		 BEGIN PERFORM pg_sleep(120); RETURN NEW; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER cc703_hold BEFORE INSERT ON run_steps FOR EACH ROW
		 WHEN (NEW.kind = 'retrieve') EXECUTE FUNCTION cc703_hold()`,
	} {
		if _, err := e.st.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		// End the sleeping backend first: its insert holds a lock that
		// DROP TRIGGER would wait on.
		if _, err := e.st.Pool().Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE wait_event = 'PgSleep' AND pid <> pg_backend_pid()`); err != nil {
			t.Errorf("end sleeping backends: %v", err)
		}
		if _, err := e.st.Pool().Exec(ctx, `DROP TRIGGER IF EXISTS cc703_hold ON run_steps`); err != nil {
			t.Errorf("drop trigger: %v", err)
		}
	}
	t.Cleanup(release)
	return release
}

// waitHeld waits until a run other than skip has at least minDone done
// steps and a backend is sleeping in the trigger; it returns the run.
func (e *cliEnv) waitHeld(t *testing.T, skip uuid.UUID, minDone int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var run uuid.UUID
		var done, sleeping int
		err := e.st.Pool().QueryRow(ctx, `
			SELECT r.id,
			       (SELECT count(*) FROM run_steps s WHERE s.run_id = r.id AND s.status = 'done'),
			       (SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'PgSleep')
			FROM close_runs r WHERE r.id <> $1 ORDER BY r.started_at DESC NULLS LAST LIMIT 1`, skip).Scan(&run, &done, &sleeping)
		if err == nil && done >= minDone && sleeping > 0 {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the run never reached the held step")
	return uuid.Nil
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// normalizedFindings are a run's findings without IDs, in report order.
func normalizedFindings(t *testing.T, st *store.Store, run uuid.UUID) string {
	t.Helper()
	fs, err := st.ListFindingsByRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fs {
		fs[i].ID, fs[i].RunID = uuid.Nil, uuid.Nil
	}
	slices.SortFunc(fs, func(a, b store.Finding) int { return strings.Compare(checks.DedupeKey(a), checks.DedupeKey(b)) })
	b, err := json.Marshal(fs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readReport(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return uuidRe.ReplaceAllString(string(b), "<id>")
}

func TestKillResume(t *testing.T) {
	e := setupCLI(t)
	ctx := context.Background()

	// The uninterrupted reference run.
	ref, err := e.runCmd(t, "close", "--company", "sharma", "--month", "2026-09")
	if err != nil {
		t.Fatalf("reference close: %v", err)
	}
	refID := uuid.MustParse(ref.RunID)
	if ref.Status != store.RunPartial || ref.Findings != 3 {
		t.Fatalf("reference run %+v, want partial with 3 findings", ref)
	}

	// The interrupted run: SIGKILL once it is held after two done steps.
	release := e.holdAtRetrieve(t)
	cmd, _, stderr := e.command(t, "close", "--company", "sharma", "--month", "2026-09")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	runID := e.waitHeld(t, refID, 2)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("killed agent exited 0\nstderr: %s", stderr)
	}
	release()

	run, err := e.st.GetCloseRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunRunning {
		t.Fatalf("killed run status %s, want running", run.Status)
	}
	stepsBefore, _ := e.st.ListSteps(ctx, runID)

	res, err := e.runCmd(t, "resume", runID.String())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.RunID != runID.String() || res.Status != store.RunPartial {
		t.Fatalf("resume result %+v, want partial", res)
	}

	// No duplicate steps; the steps done before the kill were not re-run.
	var dups int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM run_steps WHERE run_id = $1 GROUP BY run_id, kind, subject HAVING count(*) > 1) d`, runID).Scan(&dups); err != nil || dups != 0 {
		t.Errorf("duplicate steps %d, %v", dups, err)
	}
	steps, err := e.st.ListSteps(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	refSteps, _ := e.st.ListSteps(ctx, refID)
	if len(steps) != len(refSteps) {
		t.Errorf("resumed run has %d steps, the reference %d", len(steps), len(refSteps))
	}
	doneBefore := map[string]bool{}
	for _, s := range stepsBefore {
		if s.Status == store.StepDone {
			doneBefore[s.Kind+"|"+s.Subject] = true
		}
	}
	if len(doneBefore) < 2 {
		t.Errorf("only %d steps were done at the kill", len(doneBefore))
	}
	counts := map[string]int{}
	for _, s := range steps {
		counts[s.Kind+"="+s.Status]++
		if doneBefore[s.Kind+"|"+s.Subject] && s.Attempt != 1 {
			t.Errorf("step %s/%s was done before the kill but ran again (attempt %d)", s.Kind, s.Subject, s.Attempt)
		}
	}
	want := map[string]int{"router=done": 1, "check.bankrec=done": 1, "retrieve=skipped": 1,
		"explain=skipped": 3, "verify=skipped": 3, "investigate=skipped": 1, "synthesize=done": 1}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("steps %s: %d, want %d (all: %v)", k, counts[k], n, counts)
		}
	}

	// Findings and report equal the uninterrupted run's, IDs aside.
	if got, want := normalizedFindings(t, e.st, runID), normalizedFindings(t, e.st, refID); got != want {
		t.Errorf("findings differ from the uninterrupted run\n got %s\nwant %s", got, want)
	}
	if got, want := readReport(t, res.Report), readReport(t, ref.Report); got != want {
		t.Errorf("report differs from the uninterrupted run\n got:\n%s\nwant:\n%s", got, want)
	}

	// A done run can't be resumed; a partial one again is harmless.
	if _, err := e.runCmd(t, "resume", runID.String()); err != nil {
		t.Errorf("second resume of a partial run: %v", err)
	}
	if _, err := e.st.Pool().Exec(ctx, `UPDATE close_runs SET status = 'done' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runCmd(t, "resume", runID.String()); err == nil {
		t.Error("resume of a done run exited 0")
	}
}

func TestCloseCLI(t *testing.T) {
	e := setupCLI(t)
	ctx := context.Background()

	t.Run("SIGINT fails the run as cancelled", func(t *testing.T) {
		release := e.holdAtRetrieve(t)
		defer release()
		cmd, stdout, stderr := e.command(t, "close", "--company", "sharma", "--month", "2026-09")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		runID := e.waitHeld(t, uuid.Nil, 2)
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()
		var err error
		select {
		case err = <-waitErr:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("agent did not stop after SIGINT\nstderr: %s", stderr)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Fatalf("agent exit %v, want non-zero\nstderr: %s", err, stderr)
		}
		run, err := e.st.GetCloseRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != store.RunFailed || run.Error == nil || *run.Error != "cancelled" || run.FinishedAt == nil {
			t.Errorf("run %s error %v finished %v, want failed with reason cancelled", run.Status, run.Error, run.FinishedAt)
		}
		var res resultLine
		if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&res); err != nil || res.Status != store.RunFailed || res.Reason != "cancelled" {
			t.Errorf("result line %+v, %v", res, err)
		}
	})

	t.Run("bad arguments exit non-zero without a run", func(t *testing.T) {
		var before int
		_ = e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM close_runs`).Scan(&before)
		for _, args := range [][]string{
			{"close", "--company", "sharma"},
			{"close", "--company", "nobody", "--month", "2026-09"},
			{"close", "--company", "sharma", "--month", "2026-09", "--timeout", "1h"},
			{"resume", uuid.NewString()},
		} {
			if _, err := e.runCmd(t, args...); err == nil {
				t.Errorf("agent %v exited 0", args)
			}
		}
		var after int
		_ = e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM close_runs`).Scan(&after)
		if after != before {
			t.Errorf("bad arguments created %d runs", after-before)
		}
	})
}

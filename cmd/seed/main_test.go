package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

var configDir = filepath.Join("..", "..", "config", "companies")

// emptyEnv loads the config from an environment with nothing set.
func emptyEnv(required ...string) (config.Config, error) {
	return config.Load(func(string) (string, bool) { return "", false }, required...)
}

func runSeed(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, emptyEnv, requiredEnv(args), newRun(&out))
	return code, out.String(), errOut.String()
}

func TestWorldRunsWithoutERPEnv(t *testing.T) {
	code, out, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("output doesn't end in a closing brace and a newline")
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var w seed.World
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("decode world: %v", err)
	}
	if w.Company != "sharma" || w.Month != "2026-09" || len(w.Events) == 0 {
		t.Errorf("world %s %s with %d events", w.Company, w.Month, len(w.Events))
	}

	p, err := seed.LoadProfile(filepath.Join(configDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := seed.Generate(p, "2026-09", seed.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, want) {
		t.Error("printed world doesn't decode to Generate's world")
	}
	golden, err := os.ReadFile(filepath.Join("..", "..", "internal", "seed", "testdata", "world-sharma-2026-09.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(golden) {
		t.Error("printed world differs from the golden file")
	}
}

func TestWorldSmall(t *testing.T) {
	_, full, _ := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	code, small, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--small", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if len(small) >= len(full) {
		t.Errorf("--small printed %d bytes, full %d", len(small), len(full))
	}
}

func TestWorldErrors(t *testing.T) {
	cases := [][]string{
		{"world"},
		{"world", "--company", "sharma"},
		{"world", "--company", "sharma", "--month", "2026-13", "--config", configDir},
		{"world", "--company", "nobody", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "../sharma", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "sharma", "--month", "2026-09", "--config", configDir, "extra"},
		{"world", "--bogus"},
	}
	for _, args := range cases {
		code, out, _ := runSeed(t, args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		if out != "" {
			t.Errorf("%v: printed %q on failure", args, out)
		}
	}
}

func TestRequiredEnv(t *testing.T) {
	for _, sub := range []string{"world", "evidence"} {
		if got := requiredEnv([]string{sub, "--company", "sharma"}); len(got) != 0 {
			t.Errorf("%s requires %v, want nothing", sub, got)
		}
	}
	for _, args := range [][]string{nil, {"books"}, {"all"}, {"--version"}} {
		got := requiredEnv(args)
		if !slices.Equal(got, erpEnv) {
			t.Errorf("%v requires %v, want %v", args, got, erpEnv)
		}
	}
	// Other subcommands still need the ERPNext variables.
	if code, _, errOut := runSeed(t, "books"); code != 1 || !strings.Contains(errOut, config.EnvERPBaseURL) {
		t.Errorf("books with no env: exit %d, stderr %q", code, errOut)
	}
}

// erpTestEnv loads a config with the four ERP seed variables set.
func erpTestEnv(required ...string) (config.Config, error) {
	env := map[string]string{
		config.EnvERPBaseURL:       "http://localhost:8080",
		config.EnvERPSite:          "erp.localhost",
		config.EnvERPSeedAPIKey:    "seed-key",
		config.EnvERPSeedAPISecret: "seed-secret",
	}
	return config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, required...)
}

// fakeBootstrap returns a bootstrap that records the profile it got and
// reports changes created records.
func fakeBootstrap(changes int, got *seed.Profile) bootstrapFunc {
	return func(_ context.Context, c *frappe.Client, p seed.Profile) (seed.BootstrapReport, error) {
		if c == nil {
			return seed.BootstrapReport{}, errors.New("nil client")
		}
		*got = p
		return seed.BootstrapReport{
			Company: p.ERPCompany,
			Abbr:    p.Abbr,
			Counts:  map[string]*seed.Counts{"Customer": {Created: changes, Unchanged: p.Customers - changes}},
		}, nil
	}
}

func runBootstrapCmd(t *testing.T, bootstrap bootstrapFunc, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, erpTestEnv, requiredEnv(args), newRunWith(&out, bootstrap))
	return code, out.String(), errOut.String()
}

func TestBootstrapPrintsReport(t *testing.T) {
	var got seed.Profile
	code, out, errOut := runBootstrapCmd(t, fakeBootstrap(3, &got), "bootstrap", "--company", "sharma", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if got.ID != "sharma" || got.ERPCompany != "Sharma Traders Pvt Ltd" {
		t.Errorf("bootstrap got profile %q (%q)", got.ID, got.ERPCompany)
	}
	var rep seed.BootstrapReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not a report: %v\n%s", err, out)
	}
	if rep.Company != "Sharma Traders Pvt Ltd" || rep.Changes() != 3 {
		t.Errorf("report %+v", rep)
	}
	if strings.Contains(out+errOut, "seed-secret") {
		t.Error("the secret was printed")
	}
}

func TestBootstrapExpectNoChanges(t *testing.T) {
	var got seed.Profile
	code, out, errOut := runBootstrapCmd(t, fakeBootstrap(0, &got), "bootstrap", "--company", "sharma", "--config", configDir, "--expect-no-changes")
	if code != 0 {
		t.Fatalf("no changes: exit %d, stderr:\n%s", code, errOut)
	}
	if out == "" {
		t.Error("no report printed")
	}
	code, out, errOut = runBootstrapCmd(t, fakeBootstrap(2, &got), "bootstrap", "--company", "sharma", "--config", configDir, "--expect-no-changes")
	if code != 1 {
		t.Fatalf("changes: exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "2 records were created or updated") {
		t.Errorf("stderr %q", errOut)
	}
	if out == "" {
		t.Error("the report should still be printed")
	}
}

func TestBootstrapErrors(t *testing.T) {
	failing := func(context.Context, *frappe.Client, seed.Profile) (seed.BootstrapReport, error) {
		return seed.BootstrapReport{}, errors.New("boom")
	}
	var got seed.Profile
	cases := []struct {
		name string
		fn   bootstrapFunc
		args []string
	}{
		{"no company", fakeBootstrap(0, &got), []string{"bootstrap", "--config", configDir}},
		{"unknown company", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "nobody", "--config", configDir}},
		{"path in company", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "../sharma", "--config", configDir}},
		{"extra argument", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "sharma", "--config", configDir, "extra"}},
		{"bad flag", fakeBootstrap(0, &got), []string{"bootstrap", "--bogus"}},
		{"bootstrap fails", failing, []string{"bootstrap", "--company", "sharma", "--config", configDir}},
	}
	for _, tc := range cases {
		code, out, _ := runBootstrapCmd(t, tc.fn, tc.args...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", tc.name, code)
		}
		if out != "" {
			t.Errorf("%s: printed %q on failure", tc.name, out)
		}
	}
}

func TestBootstrapNeedsERPEnv(t *testing.T) {
	if got := requiredEnv([]string{"bootstrap", "--company", "sharma"}); !slices.Equal(got, erpEnv) {
		t.Errorf("bootstrap requires %v, want %v", got, erpEnv)
	}
	code, _, errOut := runSeed(t, "bootstrap", "--company", "sharma", "--config", configDir)
	if code != 1 || !strings.Contains(errOut, config.EnvERPSeedAPIKey) {
		t.Errorf("bootstrap with no env: exit %d, stderr %q", code, errOut)
	}
}

// fakeBooks returns a WriteBooks that records the world and options it got
// and reports created documents and a map.
func fakeBooks(created int, gotWorld *seed.World, gotOpt *seed.BooksOptions) booksFunc {
	return func(_ context.Context, c *frappe.Client, rep seed.BootstrapReport, w seed.World, opt seed.BooksOptions) (seed.BooksResult, error) {
		if c == nil {
			return seed.BooksResult{}, errors.New("nil client")
		}
		*gotWorld, *gotOpt = w, opt
		return seed.BooksResult{
			Company: rep.Company,
			Month:   w.Month,
			Counts:  map[string]*seed.BookCounts{seed.DocSalesInvoice: {Created: created, AlreadyPresent: 3 - created}},
			Skipped: map[string]int{seed.EventGatewaySettlement: 1},
			Map: seed.ERPMap{
				"EVT-sharma-2026-09-0001": {DocType: seed.DocSalesInvoice, Name: "SINV-26-00001"},
			},
		}, nil
	}
}

func runBooksCmd(t *testing.T, d deps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, erpTestEnv, requiredEnv(args), newRunDeps(&out, d))
	return code, out.String(), errOut.String()
}

func TestBooksPrintsResultAndWritesMap(t *testing.T) {
	var (
		gotProfile seed.Profile
		gotWorld   seed.World
		gotOpt     seed.BooksOptions
	)
	out := t.TempDir()
	d := deps{bootstrap: fakeBootstrap(0, &gotProfile), books: fakeBooks(2, &gotWorld, &gotOpt)}
	code, stdout, errOut := runBooksCmd(t, d, "books", "--company", "sharma", "--month", "2026-09", "--small",
		"--config", configDir, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	p, err := seed.LoadProfile(filepath.Join(configDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := seed.Generate(p, "2026-09", seed.Options{Small: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotWorld, want) {
		t.Error("books got another world than Generate's --small one")
	}
	if !reflect.DeepEqual(gotOpt.Suppliers, p.Suppliers) || gotOpt.StopAfter != 0 {
		t.Errorf("options %+v", gotOpt)
	}
	var res seed.BooksResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not a result: %v\n%s", err, stdout)
	}
	if res.Counts[seed.DocSalesInvoice].Created != 2 || res.Skipped[seed.EventGatewaySettlement] != 1 {
		t.Errorf("result %+v", res)
	}
	m, err := seed.LoadERPMap(filepath.Join(out, "suite-skeleton", "sharma-2026-09", seed.ERPMapFile))
	if err != nil {
		t.Fatal(err)
	}
	if m["EVT-sharma-2026-09-0001"].Name != "SINV-26-00001" {
		t.Errorf("map %v", m)
	}
	if strings.Contains(stdout+errOut, "seed-secret") {
		t.Error("the secret was printed")
	}

	// --suite picks the directory.
	code, _, errOut = runBooksCmd(t, d, "books", "--company", "sharma", "--month", "2026-09", "--suite", "suite-v1",
		"--config", configDir, "--out", out)
	if code != 0 {
		t.Fatalf("suite-v1: exit %d, stderr:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(out, "suite-v1", "sharma-2026-09", seed.ERPMapFile)); err != nil {
		t.Error(err)
	}
}

func TestBooksExpectNoChanges(t *testing.T) {
	var (
		gotProfile seed.Profile
		gotWorld   seed.World
		gotOpt     seed.BooksOptions
	)
	args := []string{"books", "--company", "sharma", "--month", "2026-09", "--config", configDir, "--out", t.TempDir(), "--expect-no-changes"}
	code, out, errOut := runBooksCmd(t, deps{fakeBootstrap(0, &gotProfile), fakeBooks(0, &gotWorld, &gotOpt)}, args...)
	if code != 0 || out == "" {
		t.Fatalf("no changes: exit %d, stderr:\n%s", code, errOut)
	}
	code, out, errOut = runBooksCmd(t, deps{fakeBootstrap(0, &gotProfile), fakeBooks(2, &gotWorld, &gotOpt)}, args...)
	if code != 1 || !strings.Contains(errOut, "2 documents were created or submitted") {
		t.Fatalf("changes: exit %d, stderr %q", code, errOut)
	}
	if out == "" {
		t.Error("the result should still be printed")
	}
}

func TestBooksNeedsBootstrapFirst(t *testing.T) {
	var (
		gotProfile seed.Profile
		gotWorld   seed.World
		gotOpt     seed.BooksOptions
	)
	out := t.TempDir()
	code, stdout, errOut := runBooksCmd(t, deps{fakeBootstrap(4, &gotProfile), fakeBooks(0, &gotWorld, &gotOpt)},
		"books", "--company", "sharma", "--month", "2026-09", "--config", configDir, "--out", out)
	if code != 1 || !strings.Contains(errOut, "run seed bootstrap first") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if stdout != "" || gotWorld.Month != "" {
		t.Error("books ran although bootstrap made changes")
	}
}

func TestBooksErrors(t *testing.T) {
	var (
		gotProfile seed.Profile
		gotWorld   seed.World
		gotOpt     seed.BooksOptions
	)
	failing := func(context.Context, *frappe.Client, seed.BootstrapReport, seed.World, seed.BooksOptions) (seed.BooksResult, error) {
		return seed.BooksResult{Map: seed.ERPMap{"EVT-1": {DocType: seed.DocSalesInvoice, Name: "SINV-1"}}}, errors.New("boom")
	}
	ok := deps{fakeBootstrap(0, &gotProfile), fakeBooks(0, &gotWorld, &gotOpt)}
	out := t.TempDir()
	base := []string{"--config", configDir, "--out", out}
	cases := []struct {
		name string
		d    deps
		args []string
	}{
		{"no month", ok, []string{"books", "--company", "sharma"}},
		{"no company", ok, []string{"books", "--month", "2026-09"}},
		{"bad month", ok, []string{"books", "--company", "sharma", "--month", "2026-13"}},
		{"unknown company", ok, []string{"books", "--company", "nobody", "--month", "2026-09"}},
		{"path in suite", ok, []string{"books", "--company", "sharma", "--month", "2026-09", "--suite", "../x"}},
		{"extra argument", ok, []string{"books", "--company", "sharma", "--month", "2026-09", "extra"}},
		{"bad flag", ok, []string{"books", "--bogus"}},
		{"books fails", deps{fakeBootstrap(0, &gotProfile), failing}, []string{"books", "--company", "sharma", "--month", "2026-09"}},
	}
	for _, tc := range cases {
		args := tc.args
		if tc.name != "bad flag" {
			args = append(slices.Clone(tc.args[:1]), append(slices.Clone(base), tc.args[1:]...)...)
		}
		code, stdout, _ := runBooksCmd(t, tc.d, args...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", tc.name, code)
		}
		if stdout != "" {
			t.Errorf("%s: printed %q on failure", tc.name, stdout)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed run wrote %v", entries)
	}
}

func TestEvidenceWritesBothFiles(t *testing.T) {
	out := t.TempDir()
	args := []string{"evidence", "--company", "sharma", "--month", "2026-09", "--small", "--config", configDir, "--out", out}
	code, stdout, errOut := runSeed(t, args...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "}\n") {
		t.Errorf("summary is not one JSON line: %q", stdout)
	}
	var sum evidenceSummary
	if err := json.Unmarshal([]byte(stdout), &sum); err != nil {
		t.Fatalf("summary: %v", err)
	}

	p, err := seed.LoadProfile(filepath.Join(configDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	prev, err := seed.Generate(p, "2026-08", seed.Options{Small: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := seed.Generate(p, "2026-09", seed.Options{Small: true})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := seed.BankLines(w)
	if err != nil {
		t.Fatal(err)
	}
	g, err := seed.BuildGSTR2B(p, "2026-09", []seed.World{prev, w}, seed.DefaultGSTR2BOptions())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Company != "sharma" || sum.Month != "2026-09" || sum.BankLines != len(lines) ||
		sum.Opening.String() != w.OpeningBank.Rupees() || sum.Closing.String() != w.ClosingBank.Rupees() ||
		sum.Invoices != len(g.Included) || sum.Late != len(g.Late) || sum.Deferred != len(g.Deferred) {
		t.Errorf("summary %+v", sum)
	}

	dir := filepath.Join(out, "sharma", "2026-09")
	golden := filepath.Join("..", "..", "internal", "seed", "testdata", "evidence")
	for file, want := range map[string]string{
		seed.BankCSVFile: filepath.Join(golden, "bank-sharma-2026-09-small.csv"),
		seed.GSTR2BFile:  filepath.Join(golden, "gstr2b-sharma-2026-09-small.json"),
	} {
		got, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // G304: test temp dir
		if err != nil {
			t.Fatal(err)
		}
		exp, err := os.ReadFile(want) //nolint:gosec // G304: repo testdata
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, exp) {
			t.Errorf("%s differs from %s", file, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("wrote %d entries, want bank.csv and gstr2b.json only", len(entries))
	}

	// A second run gives identical files.
	if code, _, errOut := runSeed(t, args...); code != 0 {
		t.Fatalf("second run: exit %d, stderr:\n%s", code, errOut)
	}
}

func TestEvidenceErrors(t *testing.T) {
	out := t.TempDir()
	base := []string{"--config", configDir, "--out", out}
	cases := [][]string{
		{"evidence"},
		{"evidence", "--company", "sharma"},
		{"evidence", "--month", "2026-09"},
		{"evidence", "--company", "sharma", "--month", "2026-13"},
		{"evidence", "--company", "sharma", "--month", "../../x"},
		{"evidence", "--company", "nobody", "--month", "2026-09"},
		{"evidence", "--company", "../sharma", "--month", "2026-09"},
		{"evidence", "--company", "sharma", "--month", "2026-09", "extra"},
		{"evidence", "--bogus"},
	}
	for _, c := range cases {
		args := append(slices.Clone(c[:1]), append(slices.Clone(base), c[1:]...)...)
		code, stdout, _ := runSeed(t, args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", c, code)
		}
		if stdout != "" {
			t.Errorf("%v: printed %q on failure", c, stdout)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed run wrote %v", entries)
	}
}

var scenariosDir = filepath.Join("..", "..", "evals", "scenarios")

func TestPlantSubcommand(t *testing.T) {
	out := t.TempDir()
	args := []string{
		"plant",
		"--suite", "suite-skeleton",
		"--company", "sharma",
		"--month", "2026-09",
		"--small",
		"--config", configDir,
		"--scenarios", scenariosDir,
		"--out", out,
	}

	code, stdout, errOut := runSeed(t, args...)
	if code != 0 {
		t.Fatalf("plant run failed: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(stdout, "suite-skeleton") {
		t.Errorf("output missing suite name: %s", stdout)
	}

	gtFile := filepath.Join(out, "scenarios", "suite-skeleton", "ground_truth", "sharma-2026-09.json")
	gt, err := seed.LoadGroundTruth(gtFile)
	if err != nil {
		t.Fatalf("LoadGroundTruth(%s): %v", gtFile, err)
	}
	if gt.Clean {
		t.Errorf("sharma-2026-09: want clean == false, got true")
	}
	if len(gt.Planted) != 3 {
		t.Errorf("planted errors: want 3, got %d", len(gt.Planted))
	}
}

func TestPlantAllTargets(t *testing.T) {
	out := t.TempDir()
	args := []string{
		"plant",
		"--suite", "suite-skeleton",
		"--small",
		"--config", configDir,
		"--scenarios", scenariosDir,
		"--out", out,
	}

	code, _, errOut := runSeed(t, args...)
	if code != 0 {
		t.Fatalf("plant run failed: exit %d, stderr: %s", code, errOut)
	}

	for _, m := range []string{"2026-08", "2026-09"} {
		gtFile := filepath.Join(out, "scenarios", "suite-skeleton", "ground_truth", "sharma-"+m+".json")
		gt, err := seed.LoadGroundTruth(gtFile)
		if err != nil {
			t.Fatalf("LoadGroundTruth(%s): %v", gtFile, err)
		}
		if m == "2026-08" && !gt.Clean {
			t.Errorf("sharma-2026-08 clean control: want clean == true")
		}
		if m == "2026-09" && gt.Clean {
			t.Errorf("sharma-2026-09 evaluated: want clean == false")
		}
	}
}

func runAllCmd(t *testing.T, d deps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, erpTestEnv, requiredEnv(args), newRunDeps(&out, d))
	return code, out.String(), errOut.String()
}

func TestAllSubcommand(t *testing.T) {
	var bootProfile seed.Profile
	var gotWorld seed.World
	var gotOpt seed.BooksOptions

	d := deps{
		bootstrap: fakeBootstrap(0, &bootProfile),
		books:     fakeBooks(0, &gotWorld, &gotOpt),
	}

	out := t.TempDir()
	args := []string{
		"all",
		"--suite", "suite-skeleton",
		"--small",
		"--config", configDir,
		"--scenarios", scenariosDir,
		"--out", out,
	}

	code, stdout, errOut := runAllCmd(t, d, args...)
	if code != 0 {
		t.Fatalf("seed all failed: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(stdout, "suite-skeleton") {
		t.Errorf("stdout missing suite: %s", stdout)
	}

	// Verify generated ERP maps
	for _, m := range []string{"2026-08", "2026-09"} {
		mapFile := filepath.Join(out, "out", "suite-skeleton", "sharma-"+m, seed.ERPMapFile)
		if _, err := seed.LoadERPMap(mapFile); err != nil {
			t.Errorf("missing or invalid erp map %s: %v", mapFile, err)
		}

		// Verify external evidence
		bankFile := filepath.Join(out, "external", "sharma", m, seed.BankCSVFile)
		if _, err := os.Stat(bankFile); err != nil {
			t.Errorf("missing bank csv %s: %v", bankFile, err)
		}
		g2bFile := filepath.Join(out, "external", "sharma", m, seed.GSTR2BFile)
		if _, err := os.Stat(g2bFile); err != nil {
			t.Errorf("missing gstr2b json %s: %v", g2bFile, err)
		}

		// Verify ground truth
		gtFile := filepath.Join(out, "evals", "scenarios", "suite-skeleton", "ground_truth", "sharma-"+m+".json")
		if _, err := seed.LoadGroundTruth(gtFile); err != nil {
			t.Errorf("missing or invalid ground truth %s: %v", gtFile, err)
		}
	}
}

func TestAllIdempotent(t *testing.T) {
	var bootProfile seed.Profile
	var gotWorld seed.World
	var gotOpt seed.BooksOptions

	d := deps{
		bootstrap: fakeBootstrap(0, &bootProfile),
		books:     fakeBooks(0, &gotWorld, &gotOpt),
	}

	out1 := t.TempDir()
	out2 := t.TempDir()

	args1 := []string{"all", "--suite", "suite-skeleton", "--small", "--config", configDir, "--scenarios", scenariosDir, "--out", out1}
	args2 := []string{"all", "--suite", "suite-skeleton", "--small", "--config", configDir, "--scenarios", scenariosDir, "--out", out2}

	if code, _, err := runAllCmd(t, d, args1...); code != 0 {
		t.Fatalf("run 1 failed: exit %d, stderr: %s", code, err)
	}
	if code, _, err := runAllCmd(t, d, args2...); code != 0 {
		t.Fatalf("run 2 failed: exit %d, stderr: %s", code, err)
	}

	// Compare outputs
	for _, m := range []string{"2026-08", "2026-09"} {
		f1 := filepath.Join(out1, "external", "sharma", m, seed.BankCSVFile)
		f2 := filepath.Join(out2, "external", "sharma", m, seed.BankCSVFile)
		b1, _ := os.ReadFile(f1)
		b2, _ := os.ReadFile(f2)
		if !bytes.Equal(b1, b2) {
			t.Errorf("bank.csv mismatch for %s", m)
		}

		g1 := filepath.Join(out1, "evals", "scenarios", "suite-skeleton", "ground_truth", "sharma-"+m+".json")
		g2 := filepath.Join(out2, "evals", "scenarios", "suite-skeleton", "ground_truth", "sharma-"+m+".json")
		gtb1, _ := os.ReadFile(g1)
		gtb2, _ := os.ReadFile(g2)
		if !bytes.Equal(gtb1, gtb2) {
			t.Errorf("ground truth mismatch for %s", m)
		}
	}
}

var (
	mapsV1  = filepath.Join("..", "..", "internal", "seed", "testdata", "maps-v1")
	truthV1 = filepath.Join("..", "..", "internal", "seed", "testdata", "ground_truth-v1")
)

// TestPlantSuiteV1 runs seed plant on suite-v1 with the maps-v1 fixtures:
// 40 errors over the six evaluated months plus the clean control, the
// files equal the seed package's fixture ground truth, a single target
// gives the same file, and two runs are byte-identical.
func TestPlantSuiteV1(t *testing.T) {
	plant := func(out string, extra ...string) plantSummary {
		t.Helper()
		args := append([]string{"plant", "--suite", "suite-v1", "--small", "--config", configDir,
			"--scenarios", scenariosDir, "--maps", mapsV1, "--out", out}, extra...)
		code, stdout, errOut := runSeed(t, args...)
		if code != 0 {
			t.Fatalf("plant %v: exit %d, stderr: %s", extra, code, errOut)
		}
		var sum plantSummary
		if err := json.Unmarshal([]byte(stdout), &sum); err != nil {
			t.Fatalf("summary %q: %v", stdout, err)
		}
		return sum
	}
	a, b := t.TempDir(), t.TempDir()
	sum := plant(a)
	if sum.ErrorsCount != 40 || len(sum.Files) != 7 || sum.Suite != "suite-v1" {
		t.Errorf("summary %+v, want 40 errors in 7 files", sum)
	}
	plant(b)
	dir := filepath.Join("scenarios", "suite-v1", "ground_truth")
	entries, err := os.ReadDir(filepath.Join(a, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Errorf("%d ground truth files, want 7", len(entries))
	}
	for _, e := range entries {
		ga, _ := os.ReadFile(filepath.Join(a, dir, e.Name()))      //nolint:gosec // G304: test temp dir
		gb, _ := os.ReadFile(filepath.Join(b, dir, e.Name()))      //nolint:gosec // G304: test temp dir
		want, err := os.ReadFile(filepath.Join(truthV1, e.Name())) //nolint:gosec // G304: repo testdata
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ga, gb) || !bytes.Equal(ga, want) {
			t.Errorf("%s: runs differ or differ from %s", e.Name(), truthV1)
		}
	}
	gt, err := seed.LoadGroundTruth(filepath.Join(a, dir, "sharma-2026-06.json"))
	if err != nil || !gt.Clean || len(gt.Planted) != 0 {
		t.Errorf("clean control: %+v, %v", gt, err)
	}

	one := t.TempDir()
	if s := plant(one, "--company", "mehta", "--month", "2026-09"); len(s.Files) != 1 {
		t.Fatalf("single target wrote %v", s.Files)
	}
	got, _ := os.ReadFile(filepath.Join(one, dir, "mehta-2026-09.json")) //nolint:gosec // G304: test temp dir
	full, _ := os.ReadFile(filepath.Join(a, dir, "mehta-2026-09.json"))  //nolint:gosec // G304: test temp dir
	if !bytes.Equal(got, full) {
		t.Error("mehta 2026-09 alone differs from the full suite")
	}
}

// TestPlantSuiteV1NeedsMaps: without ERP maps an invoice key can't be
// resolved, and plant fails rather than dropping it.
func TestPlantSuiteV1NeedsMaps(t *testing.T) {
	code, stdout, errOut := runSeed(t, "plant", "--suite", "suite-v1", "--small", "--config", configDir,
		"--scenarios", scenariosDir, "--maps", t.TempDir(), "--out", t.TempDir())
	if code != 1 || stdout != "" || !strings.Contains(errOut, "ERP map") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, errOut)
	}
}

// mapAllBooks is a WriteBooks fake that names every book-side event and
// records the worlds it got.
func mapAllBooks(got map[string]seed.World) booksFunc {
	return func(_ context.Context, c *frappe.Client, rep seed.BootstrapReport, w seed.World, _ seed.BooksOptions) (seed.BooksResult, error) {
		if c == nil {
			return seed.BooksResult{}, errors.New("nil client")
		}
		got[w.Company+" "+w.Month] = w
		m := seed.ERPMap{}
		for _, ev := range w.Events {
			if dt := seed.BookDocType(ev.Kind); dt != "" {
				m[ev.ExtID] = seed.ERPRef{DocType: dt, Name: dt + " " + ev.ExtID}
			}
		}
		return seed.BooksResult{Company: rep.Company, Month: w.Month, Counts: map[string]*seed.BookCounts{}, Map: m}, nil
	}
}

// TestAllSuiteV1 runs seed all on suite-v1 against fakes: every seeded
// month is posted and has evidence, ground truth is written for the seven
// target months only, with 40 errors whose GSTR-2B keys are in the written
// gstr2b.json, and the bootstrap reports sit next to the maps.
func TestAllSuiteV1(t *testing.T) {
	var prof seed.Profile
	posted := map[string]seed.World{}
	out := t.TempDir()
	code, stdout, errOut := runAllCmd(t, deps{bootstrap: fakeBootstrap(0, &prof), books: mapAllBooks(posted)},
		"all", "--suite", "suite-v1", "--small", "--config", configDir, "--scenarios", scenariosDir, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var sum allSummary
	if err := json.Unmarshal([]byte(stdout), &sum); err != nil || sum.MonthsCount != 12 {
		t.Errorf("summary %q: %v", stdout, err)
	}
	if len(posted) != 12 {
		t.Errorf("posted %d months, want 12", len(posted))
	}
	injected := 0
	for _, w := range posted {
		for _, ev := range w.Events {
			if strings.Contains(ev.Narration, seed.PromptInjectionText) {
				injected++
			}
		}
	}
	if injected != 4 {
		t.Errorf("%d posted remarks carry the injection, want 4", injected)
	}
	for _, c := range []string{"sharma", "mehta"} {
		if _, err := seed.LoadBootstrapReport(filepath.Join(out, "out", "suite-v1", c+"-bootstrap.json")); err != nil {
			t.Error(err)
		}
	}
	gtDir := filepath.Join(out, "evals", "scenarios", "suite-v1", "ground_truth")
	entries, err := os.ReadDir(gtDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Errorf("%d ground truth files, want 7", len(entries))
	}
	total := 0
	for _, e := range entries {
		gt, err := seed.LoadGroundTruth(filepath.Join(gtDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		total += len(gt.Planted)
		g2b, err := os.ReadFile(filepath.Join(out, "external", gt.Company, gt.Month, seed.GSTR2BFile)) //nolint:gosec // G304: test temp dir
		if err != nil {
			t.Fatal(err)
		}
		for _, pf := range gt.Planted {
			if pf.Type == seed.ErrorGSTR2BMissingInBooks || pf.Type == seed.ErrorGSTR2BAmountMismatch {
				if !bytes.Contains(g2b, []byte(`"ctin": "`+pf.Keys["supplier_gstin"]+`"`)) {
					t.Errorf("%s %s: supplier %s not in gstr2b.json", e.Name(), pf.ID, pf.Keys["supplier_gstin"])
				}
			}
			if inv := pf.Keys["invoice"]; inv != "" && !strings.HasPrefix(inv, seed.DocPurchaseInvoice+" ") {
				t.Errorf("%s %s: invoice key %q is not a Purchase Invoice", e.Name(), pf.ID, inv)
			}
		}
	}
	if total != 40 {
		t.Errorf("%d planted errors, want 40", total)
	}
}

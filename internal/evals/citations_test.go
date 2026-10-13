package evals

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func ref(doc, sec string) CitationRef { return CitationRef{DocID: doc, Section: sec} }

func cit(doc, sec string) store.Citation { return store.Citation{DocID: doc, Section: sec} }

// citCorpus is sharma's corpus: POL-1 has §1 and §2, POL-2 has §1.
func citCorpus() CorpusSections {
	return CorpusSections{Companies: map[string][]CitationRef{
		"sharma": {ref("POL-1", "§1"), ref("POL-1", "§2"), ref("POL-2", "§1")},
	}}
}

func citCase(id string, keys map[string]string, month string, expect ...CitationRef) CitationGolden {
	return CitationGolden{ID: id, Expect: expect,
		Finding: CitationFinding{Scenario: "suite-test", Company: "sharma", Month: month, Type: "t_a", Keys: keys}}
}

func citFinding(n int, inv string, cs ...store.Citation) store.Finding {
	f := fnd(n, "t_a", k("invoice", inv))
	f.Citations = cs
	return f
}

// citFixture is one finding per outcome, plus a finding outside the golden
// set that cites a valid pair, a missed case and a case whose month did not run.
func citFixture() ([]CitationGolden, []MonthInput) {
	want := ref("POL-1", "§1")
	cases := []CitationGolden{
		citCase("F01", k("invoice", "I1"), "2026-07", want),                     // right
		citCase("F02", k("invoice", "I2"), "2026-07", want),                     // wrong section
		citCase("F03", k("invoice", "I3"), "2026-07", want),                     // wrong document
		citCase("F04", k("invoice", "I4"), "2026-07", want),                     // unsupported
		citCase("F05", k("invoice", "I5"), "2026-07", want),                     // right + invalid pair
		citCase("F06", k("invoice", "I6"), "2026-07", want),                     // uncited
		citCase("F07", k("invoice", "I7"), "2026-07", want),                     // missed
		citCase("F08", k("invoice", "I8"), "2026-10", want),                     // month not run
		citCase("F09", k("invoice", "I9"), "2026-07", want, ref("POL-1", "§2")), // any-of: second entry
	}
	m := monthIn("sharma", "2026-07", false, GroundTruth{},
		citFinding(1, "I1", cit("POL-1", "§1")),
		citFinding(2, "I2", cit("POL-1", "§2")),
		citFinding(3, "I3", cit("POL-2", "§1")),
		citFinding(4, "I4", cit("POL-1", "§9")),
		citFinding(5, "I5", cit("POL-1", "§1"), cit("POL-3", "§7")),
		citFinding(6, "I6"),
		citFinding(9, "I9", cit("POL-1", "§2")),
		citFinding(10, "I10", cit("POL-2", "§1")), // not golden
	)
	return cases, []MonthInput{m}
}

func TestCitationsScore(t *testing.T) {
	cases, months := citFixture()
	got := ScoreCitations(cases, months, citCorpus())
	want := map[string]string{"F01": CitationRight, "F02": CitationWrongSection, "F03": CitationWrongDocument,
		"F04": CitationUnsupported, "F05": CitationUnsupported, "F06": CitationUncited, "F07": CitationMissed,
		"F08": CitationNotRun, "F09": CitationRight}
	var order []string
	for _, c := range got.Cases {
		order = append(order, c.ID)
		if c.Outcome != want[c.ID] {
			t.Errorf("%s: outcome %s, want %s", c.ID, c.Outcome, want[c.ID])
		}
		if (c.Outcome == CitationRight) != (c.Repro == "") {
			t.Errorf("%s: repro %q for outcome %s", c.ID, c.Repro, c.Outcome)
		}
		if c.Evidence == "" {
			t.Errorf("%s: no evidence pointer", c.ID)
		}
	}
	if !slices.Equal(order, []string{"F01", "F02", "F03", "F04", "F05", "F06", "F07", "F08", "F09"}) {
		t.Errorf("order %v", order)
	}
	if !got.Measured || got.Reason != "" {
		t.Errorf("measured %t reason %q", got.Measured, got.Reason)
	}
	// 8 cited pairs on findings 1-6, 9 and 10: I1 1, I2 1, I3 1, I4 1, I5 2, I9 1, I10 1 = 8; invalid: I4 and I5's POL-3 = 2.
	if got.Validity != NewRatio(6, 8) {
		t.Errorf("validity %v, want 6/8", got.Validity)
	}
	// Applicable: F01-F06 and F09 = 7; cited = 6 (F06 is uncited); right = F01 and F09.
	if got.Cited != NewRatio(6, 7) || got.Precision != NewRatio(2, 6) {
		t.Errorf("cited %v precision %v", got.Cited, got.Precision)
	}
	if c := got.Cases[4]; len(c.Cited) != 2 || c.FindingID != fid(5).String() {
		t.Errorf("F05 %+v", c)
	}
	if c := got.Cases[1]; c.Repro != "go run ./cmd/eval run --suite suite-test --only sharma:2026-07" ||
		c.Evidence != "r/sharma-2026-07.json#"+fid(2).String() {
		t.Errorf("F02 repro %q evidence %q", c.Repro, c.Evidence)
	}
}

func TestCitationsOrderStable(t *testing.T) {
	cases, months := citFixture()
	a := ScoreCitations(cases, months, citCorpus())
	slices.Reverse(cases)
	fs := months[0].Result.Findings
	slices.Reverse(fs)
	b := ScoreCitations(cases, months, citCorpus())
	if !reflect.DeepEqual(a, b) {
		t.Errorf("input order changed the result:\n%+v\n%+v", a, b)
	}
}

func TestCitationsMatchByKeysNotText(t *testing.T) {
	cases, months := citFixture()
	months[0].Result.Findings[0].Title = "POL-1 §1 spread annual costs"
	months[0].Result.Findings[0].Keys = k("invoice", "OTHER")
	got := ScoreCitations(cases, months, citCorpus())
	if got.Cases[0].Outcome != CitationMissed {
		t.Errorf("a finding with other keys matched by text: %+v", got.Cases[0])
	}
}

func TestCitationsUnmeasured(t *testing.T) {
	cases, months := citFixture()
	got := ScoreCitations(cases[6:8], months, citCorpus()) // missed + not run only
	if got.Measured || got.Reason == "" || got.Validity.Den != 0 || got.Precision.Den != 0 || got.Cited.Den != 0 || len(got.Cases) != 2 {
		t.Errorf("no applicable case: %+v", got)
	}
	if u := unmeasuredCitations("x"); u.Measured || u.Validity.Percent != "n/a" || u.Cases == nil {
		t.Errorf("unmeasured %+v", u)
	}
}

func TestCitationsCorpusIsPerCompany(t *testing.T) {
	cases, months := citFixture()
	got := ScoreCitations(cases[:1], months, CorpusSections{Companies: map[string][]CitationRef{"other": {ref("POL-1", "§1")}}})
	if got.Cases[0].Outcome != CitationUnsupported {
		t.Errorf("another company's corpus validated a citation: %+v", got.Cases[0])
	}
}

func TestCitationsLoadGolden(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "g.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const ok = `- id: F01
  finding: {scenario: suite-test, company: sharma, month: "2026-07", type: t_a, keys: {invoice: "I1"}}
  expect: [{doc_id: POL-1, section: "§1"}]
`
	cs, err := LoadCitationGolden(write("# comment\n" + ok))
	if err != nil || len(cs) != 1 || cs[0].Finding.Keys["invoice"] != "I1" || cs[0].Expect[0] != ref("POL-1", "§1") {
		t.Fatalf("load: %+v, %v", cs, err)
	}
	for name, body := range map[string]string{
		"unknown field": strings.Replace(ok, "expect:", "extra: 1\n  expect:", 1),
		"empty expect":  strings.Replace(ok, `[{doc_id: POL-1, section: "§1"}]`, "[]", 1),
		"no keys":       strings.Replace(ok, `keys: {invoice: "I1"}`, "keys: {}", 1),
		"empty key":     strings.Replace(ok, `"I1"`, `""`, 1),
		"duplicate id":  ok + ok,
		"no type":       strings.Replace(ok, "type: t_a, ", "", 1),
	} {
		if _, err := LoadCitationGolden(write(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := LoadCitationGolden(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file: no error")
	}
}

func TestCitationsScoreOptions(t *testing.T) {
	cases, months := citFixture()
	dir := t.TempDir()
	golden := filepath.Join(dir, "citations.yaml")
	var b strings.Builder
	for _, c := range cases[:2] {
		b.WriteString("- id: " + c.ID + "\n  finding: {scenario: suite-test, company: sharma, month: \"2026-07\", type: t_a, keys: {invoice: " + c.Finding.Keys["invoice"] + "}}\n  expect: [{doc_id: POL-1, section: \"§1\"}]\n")
	}
	if err := os.WriteFile(golden, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// A recorded corpus, as eval run --record writes it.
	env := newFxEnv(t)
	rec, err := NewRecorder(env.root, "suite-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Abort)
	rec.UseMonth("sharma", "2026-09")
	if err := rec.RecordCorpus("sharma", citCorpus().Companies["sharma"]); err != nil {
		t.Fatal(err)
	}
	env.suite.Name = "suite-test"
	if _, _, err := rec.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months, SeederCommit: "dev-test"}); err != nil {
		t.Fatal(err)
	}

	got, err := scoreCitationsFor(ScoreOptions{CitationsPath: golden, FixturesDir: env.root}, "suite-test", months)
	if err != nil || !got.Measured || got.Precision != NewRatio(1, 2) || len(got.Cases) != 2 {
		t.Fatalf("measured: %+v, %v", got, err)
	}
	for name, opts := range map[string]ScoreOptions{
		"no golden path":    {FixturesDir: env.root},
		"golden missing":    {CitationsPath: filepath.Join(dir, "none.yaml"), FixturesDir: env.root},
		"no fixtures dir":   {CitationsPath: golden},
		"no corpus fixture": {CitationsPath: golden, FixturesDir: filepath.Join(dir, "nofx")},
	} {
		got, err := scoreCitationsFor(opts, "suite-test", months)
		if err != nil || got.Measured || got.Reason == "" || got.Validity.Den != 0 {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if got, err := scoreCitationsFor(ScoreOptions{CitationsPath: golden, FixturesDir: env.root}, "suite-skeleton", months); err != nil || got.Measured ||
		!strings.Contains(got.Reason, "suite-skeleton") {
		t.Errorf("a suite with no case: %+v, %v", got, err)
	}
	// A tampered corpus file fails hash checking: an error, not unmeasured.
	if err := os.WriteFile(filepath.Join(env.root, "suite-test", CorpusSectionsFile), []byte(`{"companies":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scoreCitationsFor(ScoreOptions{CitationsPath: golden, FixturesDir: env.root}, "suite-test", months); err == nil {
		t.Error("tampered corpus accepted")
	}
}

func TestCitationsReport(t *testing.T) {
	cases, months := citFixture()
	s := mustScore(t, months[0])
	s.Citations = ScoreCitations(cases, months, citCorpus())
	got := RenderReport(s)
	for _, want := range []string{"## Citations", "| Validity | 6/8 (75.0%) |", "| Precision | 2/6 (33.3%) |", "| Cited | 6/7 (85.7%) |",
		"| F02 | wrong_section |", "| F03 | wrong_document |", "| F04 | unsupported |", "| F06 | uncited | (none) |", "| F07 | missed |", "| F08 | not_run |"} {
		if !strings.Contains(got, want) {
			t.Errorf("score.md lacks %q", want)
		}
	}
	golden := filepath.Join(scoreTestdata, "golden", "citations.md")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("score.md differs from %s:\n%s", golden, got)
	}
	// Not measured never reads as 100%.
	s.Citations = unmeasuredCitations("no corpus fixture")
	if md := RenderReport(s); !strings.Contains(md, "Citations: not measured: no corpus fixture") || strings.Contains(md, "| Validity |") {
		t.Errorf("unmeasured section:\n%s", md)
	}
}

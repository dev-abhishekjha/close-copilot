package evals

// Citation scoring (CC-806b). The scorer grades the citations a run's
// findings carry against the owner's golden file (evals/golden/citations.yaml,
// CC-907) and the ingested corpus. Findings are matched to golden cases by
// the ground-truth keys (target.matches), never by text. No model call.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// Citation file names and defaults.
const (
	// DefaultCitationsPath is the golden citations file.
	DefaultCitationsPath = "evals/golden/citations.yaml"
	// CorpusSectionsFile is the corpus fixture, <fixtures-dir>/<suite>/corpus-sections.json.
	CorpusSectionsFile = "corpus-sections.json"
)

// Per-case citation outcomes.
const (
	CitationRight          = "right"
	CitationWrongSection   = "wrong_section"
	CitationWrongDocument  = "wrong_document"
	CitationUnsupported    = "unsupported"
	CitationUncited        = "uncited"
	CitationMissed         = "missed"
	CitationNotRun         = "not_run"
	reasonCitationsNoGold  = "no golden citations file given (eval score --citations)"
	reasonCitationsNoCorp  = "no corpus fixture: record the suite with the agent on (eval run --record) to write corpus-sections.json"
	reasonCitationsNoCases = "no golden case is applicable: none matches a finding of this run"
)

// CitationRef is a (doc_id, section) pair.
type CitationRef struct {
	DocID   string `json:"doc_id" yaml:"doc_id"`
	Section string `json:"section" yaml:"section"`
}

func (c CitationRef) String() string { return c.DocID + " " + c.Section }

// CitationFinding names the finding a golden case is about, by ground-truth keys.
type CitationFinding struct {
	Scenario string            `yaml:"scenario"`
	Company  string            `yaml:"company"`
	Month    string            `yaml:"month"`
	Type     string            `yaml:"type"`
	Keys     map[string]string `yaml:"keys"`
}

// CitationGolden is one golden case. Expect is any-of.
type CitationGolden struct {
	ID      string          `yaml:"id"`
	Finding CitationFinding `yaml:"finding"`
	Expect  []CitationRef   `yaml:"expect"`
}

// LoadCitationGolden reads the golden file strictly: unknown fields, an
// empty expect, a case with no finding keys, an empty key value, a missing
// finding field and duplicate ids are errors. A missing file is an error
// too; the caller decides whether that means "not measured".
func LoadCitationGolden(path string) ([]CitationGolden, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("evals: citations golden: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cases []CitationGolden
	if err := dec.Decode(&cases); err != nil {
		return nil, fmt.Errorf("evals: citations golden %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("evals: citations golden %s: more than one YAML document", path)
	}
	seen := map[string]bool{}
	for i, c := range cases {
		where := fmt.Sprintf("evals: citations golden %s: case %d (%.40q)", path, i+1, c.ID)
		switch {
		case strings.TrimSpace(c.ID) == "":
			return nil, fmt.Errorf("%s: empty id", where)
		case seen[c.ID]:
			return nil, fmt.Errorf("%s: duplicate id", where)
		case c.Finding.Scenario == "" || c.Finding.Company == "" || c.Finding.Month == "" || c.Finding.Type == "":
			return nil, fmt.Errorf("%s: finding needs scenario, company, month and type", where)
		case len(c.Finding.Keys) == 0:
			return nil, fmt.Errorf("%s: finding has no keys", where)
		case len(c.Expect) == 0:
			return nil, fmt.Errorf("%s: empty expect", where)
		}
		for k, v := range c.Finding.Keys {
			if strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("%s: key %.40q is empty", where, k)
			}
		}
		for _, e := range c.Expect {
			if e.DocID == "" || e.Section == "" {
				return nil, fmt.Errorf("%s: expect entry needs doc_id and section", where)
			}
		}
		seen[c.ID] = true
	}
	return cases, nil
}

// CorpusSections is the set of (doc_id, section) pairs a company may cite:
// its own documents' plus the shared ones, as Store.CitationExists reads
// doc_chunks. It is the content of corpus-sections.json.
type CorpusSections struct {
	// Companies maps a company ID to its sorted, distinct pairs.
	Companies map[string][]CitationRef `json:"companies"`
}

// Has reports whether company may cite (docID, section).
func (c CorpusSections) Has(company, docID, section string) bool {
	for _, p := range c.Companies[company] {
		if p.DocID == docID && p.Section == section {
			return true
		}
	}
	return false
}

// Empty reports whether the set holds no pair.
func (c CorpusSections) Empty() bool {
	for _, ps := range c.Companies {
		if len(ps) > 0 {
			return false
		}
	}
	return true
}

// normalise sorts and de-duplicates each company's pairs.
func (c CorpusSections) normalise() CorpusSections {
	out := CorpusSections{Companies: map[string][]CitationRef{}}
	for k, ps := range c.Companies {
		cp := slices.Clone(ps)
		sort.Slice(cp, func(a, b int) bool {
			if cp[a].DocID != cp[b].DocID {
				return cp[a].DocID < cp[b].DocID
			}
			return cp[a].Section < cp[b].Section
		})
		out.Companies[k] = slices.Compact(cp)
	}
	return out
}

// LoadCorpusSections reads <fixturesDir>/<suite>/corpus-sections.json
// through the suite's fixture index: the file must be listed there and
// match its sha256. It needs no database and no replay checks. A missing
// index or corpus file wraps ErrFixtureMissing.
func LoadCorpusSections(fixturesDir, suite string) (CorpusSections, error) {
	dir := filepath.Join(fixturesDir, suite)
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, FixtureIndexFile)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CorpusSections{}, fmt.Errorf("%w: no fixtures index in %s", ErrFixtureMissing, dir)
		}
		return CorpusSections{}, fmt.Errorf("evals: corpus sections: %w", err)
	}
	var idx FixtureIndex
	if err := decodeStrict(raw, &idx); err != nil {
		return CorpusSections{}, fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, dir, err)
	}
	want, ok := idx.Files[CorpusSectionsFile]
	if !ok {
		return CorpusSections{}, fmt.Errorf("%w: %s lists no %s", ErrFixtureMissing, dir, CorpusSectionsFile)
	}
	body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, CorpusSectionsFile)))
	if err != nil {
		return CorpusSections{}, fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, CorpusSectionsFile, err)
	}
	if got := sha256Hex(body); got != want {
		return CorpusSections{}, fmt.Errorf("%w: %s has sha256 %s, the index says %s", ErrFixturesInvalid, CorpusSectionsFile, got, want)
	}
	return decodeCorpus(body)
}

func decodeCorpus(body []byte) (CorpusSections, error) {
	var c CorpusSections
	if err := decodeStrict(body, &c); err != nil {
		return CorpusSections{}, fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, CorpusSectionsFile, err)
	}
	if c.Companies == nil {
		c.Companies = map[string][]CitationRef{}
	}
	return c, nil
}

// CitationCase is one golden case's outcome.
type CitationCase struct {
	ID        string        `json:"id"`
	Company   string        `json:"company"`
	Month     string        `json:"month"`
	Type      string        `json:"type"`
	Outcome   string        `json:"outcome"`
	FindingID string        `json:"finding_id"`
	Cited     []CitationRef `json:"cited"`
	Expected  []CitationRef `json:"expected"`
	// Repro is set on every case that is not right; Evidence on all.
	Repro    string `json:"repro"`
	Evidence string `json:"evidence"`
}

// Citations is the citation section of score.json.
//
// A golden case is applicable when its month ran and one of that month's
// findings matches its type and keys (target.matches: ground-truth keys,
// never text). A case whose month ran but whose finding was not produced
// is missed and one whose month is not in the run is not_run; both are
// listed and neither is scored (recall already measures a missed finding).
//
//   - Validity is the cited pairs that exist in the corpus over every cited
//     pair on every finding of every scored month (not only golden ones).
//   - Precision is the applicable findings with at least one citation in the
//     case's any-of expect list, over the applicable findings with at least
//     one citation. A finding that cites a pair outside the corpus fails it.
//   - Cited is the applicable findings with at least one citation over the
//     applicable findings, so a model that cites nothing is visible.
//
// One outcome per case: unsupported when any cited pair is not in the
// corpus, else right, else wrong_section (a cited doc_id is in expect, no
// listed section), else wrong_document, with uncited, missed and not_run
// as above. With Measured false every ratio is 0/0 and Reason says why;
// that never reads as 100%.
type Citations struct {
	Measured  bool           `json:"measured"`
	Reason    string         `json:"reason"`
	Validity  Ratio          `json:"validity"`
	Precision Ratio          `json:"precision"`
	Cited     Ratio          `json:"cited"`
	Cases     []CitationCase `json:"cases"`
}

// unmeasuredCitations is the score of a run that could not be graded.
func unmeasuredCitations(reason string) Citations {
	return Citations{Reason: reason, Validity: NewRatio(0, 0), Precision: NewRatio(0, 0), Cited: NewRatio(0, 0), Cases: []CitationCase{}}
}

// ScoreCitations grades months against the golden cases and the corpus. It
// is pure: no I/O, no model, deterministic (cases in id order). Callers
// pass only the cases of the run's suite. With no applicable case the
// result is unmeasured, with the cases still listed.
func ScoreCitations(cases []CitationGolden, months []MonthInput, corpus CorpusSections) Citations {
	byMonth := map[string]MonthInput{}
	for _, m := range months {
		byMonth[m.Entry.Company+"-"+m.Entry.Month] = m
	}
	var validNum, validDen int64
	for _, m := range months {
		if m.Result == nil {
			continue
		}
		for _, f := range m.Result.Findings {
			for _, c := range f.Citations {
				validDen++
				if corpus.Has(m.Entry.Company, c.DocID, c.Section) {
					validNum++
				}
			}
		}
	}

	sorted := slices.Clone(cases)
	sort.SliceStable(sorted, func(a, b int) bool { return idLess(sorted[a].ID, sorted[b].ID) })
	out := Citations{Cases: []CitationCase{}}
	var applicable, cited, right int64
	for _, g := range sorted {
		expected := make([]CitationRef, 0, len(g.Expect))
		for _, e := range g.Expect {
			expected = append(expected, CitationRef{DocID: e.DocID, Section: e.Section})
		}
		cc := CitationCase{ID: g.ID, Company: g.Finding.Company, Month: g.Finding.Month, Type: g.Finding.Type,
			Cited: []CitationRef{}, Expected: expected,
			Repro: reproRun(g.Finding.Scenario, g.Finding.Company, g.Finding.Month)}
		m, ran := byMonth[g.Finding.Company+"-"+g.Finding.Month]
		var f *store.Finding
		if ran && m.Result != nil {
			t := target{typ: g.Finding.Type, keys: g.Finding.Keys}
			for _, cand := range sortFindings(m.Result.Findings) {
				if t.matches(cand) {
					f = &cand
					break
				}
			}
		}
		switch {
		case !ran:
			cc.Outcome = CitationNotRun
			cc.Evidence = DefaultCitationsPath + "#" + g.ID
		case f == nil:
			cc.Outcome = CitationMissed
			cc.Evidence = nonEmpty(m.ResultPath, ManifestFile) + "#findings"
		default:
			applicable++
			cc.FindingID = f.ID.String()
			cc.Evidence = nonEmpty(m.ResultPath, ManifestFile) + "#" + cc.FindingID
			for _, c := range f.Citations {
				cc.Cited = append(cc.Cited, CitationRef{DocID: c.DocID, Section: c.Section})
			}
			if len(cc.Cited) == 0 {
				cc.Outcome = CitationUncited
				break
			}
			cited++
			cc.Outcome = judgeCitation(cc.Cited, g.Finding.Company, expected, corpus)
			if cc.Outcome == CitationRight {
				right++
			}
		}
		if cc.Outcome == CitationRight {
			cc.Repro = ""
		}
		out.Cases = append(out.Cases, cc)
	}
	out.Validity = NewRatio(validNum, validDen)
	out.Precision = NewRatio(right, cited)
	out.Cited = NewRatio(cited, applicable)
	if applicable == 0 {
		u := unmeasuredCitations(reasonCitationsNoCases)
		u.Cases = out.Cases
		return u
	}
	out.Measured = true
	return out
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// judgeCitation picks the one outcome of a cited finding.
func judgeCitation(cited []CitationRef, company string, expected []CitationRef, corpus CorpusSections) string {
	for _, c := range cited {
		if !corpus.Has(company, c.DocID, c.Section) {
			return CitationUnsupported
		}
	}
	docOK := false
	for _, c := range cited {
		for _, e := range expected {
			if c == e {
				return CitationRight
			}
			if c.DocID == e.DocID {
				docOK = true
			}
		}
	}
	if docOK {
		return CitationWrongSection
	}
	return CitationWrongDocument
}

// marshalCorpus is the corpus file body.
func marshalCorpus(c CorpusSections) ([]byte, error) {
	b, err := marshalSorted(c.normalise())
	if err != nil {
		return nil, fmt.Errorf("evals: corpus sections: %w", err)
	}
	return b, nil
}

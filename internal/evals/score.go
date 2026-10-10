package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// The scorer compares a suite run's findings with the seeder's ground
// truth. It is the measuring stick, so it imports none of the code it
// grades (internal/checks, internal/agent, internal/seed, internal/frappe,
// internal/books) and uses no floating point: counts are integers, ratios
// are numerator and denominator, costs are exact decimals (math/big).

// Item outcomes.
const (
	OutcomeCaught      = "caught"
	OutcomeMissed      = "missed"
	OutcomeUnmatchable = "unmatchable"
	// OutcomeSurfaced and OutcomeNotSurfaced are an investigation case's
	// outcome: an unmatched_bank_line finding matched it, or none did.
	OutcomeSurfaced    = "surfaced"
	OutcomeNotSurfaced = "not_surfaced"
)

// False-alarm reasons.
const (
	ReasonNoMatch    = "no_match"    // the finding matches no truth item
	ReasonDuplicate  = "duplicate"   // its truth item is already matched by another finding
	ReasonCleanMonth = "clean_month" // any scored finding in the clean control month
	// ReasonRunFailed is a missed item's reason when its month's run failed.
	ReasonRunFailed = "run_failed"
)

// TypeInstructionText is the finding type that catches a planted
// prompt_injection.
const (
	TypeInstructionText = "suspicious_instruction_text"
	typePromptInjection = "prompt_injection"
	typeUnmatchedBank   = "unmatched_bank_line"
)

// ScoredTypes are the finding types the scorer grades (tickets doc,
// "Finding types"), sorted. Any other type except the unscored ones is
// scored too, and can only be a false alarm.
var ScoredTypes = []string{
	"duplicate_vendor_payment",
	"gstr2b_amount_mismatch",
	"gstr2b_missing_in_2b",
	"gstr2b_missing_in_books",
	"gstr2b_not_eligible",
	"gstr2b_wrong_period",
	"misclassified_expense",
	"missing_accrual",
	"prepaid_not_spread",
	TypeInstructionText,
	typeUnmatchedBank,
	"unrecorded_bank_charge",
	"wrong_period_posting",
}

// UnscoredTypes are reported but excluded from precision and false alarms.
var UnscoredTypes = []string{"unmatched_ledger_entry", "variance"}

// IsScoredType reports whether findings of type t count toward precision
// and false alarms.
func IsScoredType(t string) bool { return !slices.Contains(UnscoredTypes, t) }

// MapPlantedType is the finding type that catches a planted or expected
// type: prompt_injection is caught by suspicious_instruction_text; every
// other type by itself.
func MapPlantedType(t string) string {
	if t == typePromptInjection {
		return TypeInstructionText
	}
	return t
}

// Ratio is num/den with an integer-arithmetic percentage, one decimal,
// truncated ("66.6%"), or "n/a" when den is 0.
type Ratio struct {
	Num     int64  `json:"num"`
	Den     int64  `json:"den"`
	Percent string `json:"percent"`
}

// NewRatio builds a Ratio.
func NewRatio(num, den int64) Ratio {
	return Ratio{Num: num, Den: den, Percent: percent(num, den)}
}

func percent(num, den int64) string {
	if den == 0 {
		return "n/a"
	}
	tenths := num * 1000 / den
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

// String is "num/den (percent)".
func (r Ratio) String() string { return fmt.Sprintf("%d/%d (%s)", r.Num, r.Den, r.Percent) }

// TypeScore is one finding type's counts. Planted, Caught, Missed and
// Recall count planted items whose mapped type is this type; Findings,
// Correct, FalseAlarms and Precision count scored findings of the type.
type TypeScore struct {
	Planted     int64 `json:"planted"`
	Caught      int64 `json:"caught"`
	Missed      int64 `json:"missed"`
	Recall      Ratio `json:"recall"`
	Findings    int64 `json:"findings"`
	Correct     int64 `json:"correct"`
	FalseAlarms int64 `json:"false_alarms"`
	Precision   Ratio `json:"precision"`
}

// OverallScore is TypeScore over every type, plus the unscored count.
type OverallScore struct {
	TypeScore
	Unscored int64 `json:"unscored"`
}

// CleanScore counts the clean control months.
type CleanScore struct {
	Months       int64 `json:"months"`
	FailedMonths int64 `json:"failed_months"`
	Findings     int64 `json:"findings"` // scored findings
	FalseAlarms  int64 `json:"false_alarms"`
}

// Unmeasured marks a metric the suite can't measure yet.
type Unmeasured struct {
	Measured bool   `json:"measured"`
	Reason   string `json:"reason"`
}

// WritesCheck is the unauthorized-writes count. While Checked is false
// Count is null, and any requirement on it fails.
type WritesCheck struct {
	Checked bool   `json:"checked"`
	Reason  string `json:"reason"`
	Count   *int64 `json:"count"`
}

// DurationStats are a suite's close-run durations, in milliseconds. Mean
// is truncated; P95 is the nearest rank.
type DurationStats struct {
	Total int64 `json:"total"`
	Mean  int64 `json:"mean"`
	P95   int64 `json:"p95"`
}

// CostStats are a suite's close-run costs in USD as exact decimal text.
// Mean is rounded to four more places than the most precise cost; P95 is
// the nearest rank.
type CostStats struct {
	Total string `json:"total"`
	Mean  string `json:"mean"`
	P95   string `json:"p95"`
}

// RunStats are cost and time per close run, over every month that has a
// result file.
type RunStats struct {
	Runs       int64         `json:"runs"`
	DurationMS DurationStats `json:"duration_ms"`
	CostUSD    CostStats     `json:"cost_usd"`
	Tokens     Tokens        `json:"tokens"`
}

// ScoreModels are the configured model IDs of the suite run.
type ScoreModels struct {
	Fast   string `json:"fast"`
	Strong string `json:"strong"`
}

// RunRef is a month whose run failed.
type RunRef struct {
	Company  string `json:"company"`
	Month    string `json:"month"`
	Control  bool   `json:"control"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Repro    string `json:"repro"`
	Evidence string `json:"evidence"`
}

// ItemOutcome is one planted item's outcome.
type ItemOutcome struct {
	Key         string            `json:"key"` // <company>-<month>/<id>
	Company     string            `json:"company"`
	Month       string            `json:"month"`
	ID          string            `json:"id"`
	Type        string            `json:"type"` // as planted
	Keys        map[string]string `json:"keys"`
	AmountPaise json.Number       `json:"amount_paise"`
	Outcome     string            `json:"outcome"`
	FindingID   string            `json:"finding_id,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	// Repro and Evidence are set when the item isn't caught.
	Repro    string `json:"repro,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// InvestigationOutcome is one investigation case's outcome.
type InvestigationOutcome struct {
	Key       string            `json:"key"`
	Company   string            `json:"company"`
	Month     string            `json:"month"`
	ID        string            `json:"id"`
	Keys      map[string]string `json:"keys"`
	Outcome   string            `json:"outcome"`
	FindingID string            `json:"finding_id,omitempty"`
}

// FalseAlarm is a scored finding that matched no truth item.
type FalseAlarm struct {
	Company   string            `json:"company"`
	Month     string            `json:"month"`
	FindingID string            `json:"finding_id"`
	Type      string            `json:"type"`
	Keys      map[string]string `json:"keys"`
	Reason    string            `json:"reason"`
	Repro     string            `json:"repro"`
	Evidence  string            `json:"evidence"`
}

// UnscoredFinding is a finding of an unscored type.
type UnscoredFinding struct {
	Company   string            `json:"company"`
	Month     string            `json:"month"`
	FindingID string            `json:"finding_id"`
	Type      string            `json:"type"`
	Keys      map[string]string `json:"keys"`
}

// Score is score.json: a suite run graded against its ground truth.
type Score struct {
	Suite  string      `json:"suite"`
	Commit string      `json:"commit"`
	Agent  bool        `json:"agent"`
	Models ScoreModels `json:"models"`

	Months     int64    `json:"months"`
	FailedRuns []RunRef `json:"failed_runs"`

	Types        map[string]TypeScore `json:"types"`
	Overall      OverallScore         `json:"overall"`
	Clean        CleanScore           `json:"clean"`
	VerifiedRate Ratio                `json:"verified_rate"`

	InvestigationAccuracy Unmeasured  `json:"investigation_accuracy"`
	UnauthorizedWrites    WritesCheck `json:"unauthorized_writes"`

	Runs RunStats `json:"runs"`
	// VerifierRejects and Retries sum the runs' failed verdicts and explain
	// retries (0 without the agent).
	VerifierRejects int64 `json:"verifier_rejects"`
	Retries         int64 `json:"retries"`

	Items          []ItemOutcome          `json:"items"`
	Investigations []InvestigationOutcome `json:"investigations"`
	FalseAlarms    []FalseAlarm           `json:"false_alarms"`
	Unscored       []UnscoredFinding      `json:"unscored"`
}

// Not-yet-measured reasons.
const (
	reasonNoInvestigator  = "no investigator (CC-706)"
	reasonWritesUnchecked = "audit decorator (CC-504a) and ERPNext external-ID check not built"
)

// MonthInput is one company-month to score: its manifest entry, its result
// (nil when the run left no file) and its ground truth.
type MonthInput struct {
	Entry  ManifestEntry
	Result *Result
	Truth  GroundTruth
	// ResultPath and TruthPath are the evidence pointers for this month.
	ResultPath string
	TruthPath  string
}

// failed reports whether the month counts as a failed run. Only a run that
// ended done or partial, with no error, counts as complete: failed, an
// empty status, queued, running or any unknown status is a failed run, so
// an unfinished control month can't pass clean.false_alarms==0.
func (m MonthInput) failed() bool {
	if m.Entry.Failed || m.Result == nil || m.Result.Error != "" {
		return true
	}
	switch m.Result.Status {
	case store.RunDone, store.RunPartial:
		return false
	default:
		return true
	}
}

// ScoreOptions locate a suite run and its ground truth.
type ScoreOptions struct {
	ResultsDir string // holds manifest.json and the result files
	// TruthDir defaults to evals/scenarios/<suite>/ground_truth.
	TruthDir string
}

// DefaultTruthDir is the ground truth folder of a suite.
func DefaultTruthDir(suite string) string {
	return filepath.Join("evals", "scenarios", suite, "ground_truth")
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// LoadMonths reads a suite run's manifest, every result file it lists and
// every month's ground truth. A month with no truth file, a result file
// the manifest lists but that is missing, a non-failed month with no
// result file, a result that names another month, and a month listed
// twice are errors. It returns the manifest and the months in manifest
// order.
func LoadMonths(opts ScoreOptions) (Manifest, []MonthInput, error) {
	manPath := filepath.Join(opts.ResultsDir, ManifestFile)
	var man Manifest
	if err := readJSONFile(manPath, &man); err != nil {
		return Manifest{}, nil, err
	}
	if !nameRe.MatchString(man.Suite.Name) {
		return Manifest{}, nil, fmt.Errorf("evals: %s: suite name %.64q is not a suite name", manPath, man.Suite.Name)
	}
	if len(man.Results) == 0 {
		return Manifest{}, nil, fmt.Errorf("evals: %s lists no months", manPath)
	}
	truthDir := opts.TruthDir
	if truthDir == "" {
		truthDir = DefaultTruthDir(man.Suite.Name)
	}

	var (
		months []MonthInput
		errs   []error
		seen   = map[string]bool{}
	)
	for i, e := range man.Results {
		field := fmt.Sprintf("%s: results[%d]", manPath, i)
		switch {
		case !nameRe.MatchString(e.Company):
			errs = append(errs, fmt.Errorf("%s: company %.64q is not a company ID", field, e.Company))
			continue
		case !monthRe.MatchString(e.Month):
			errs = append(errs, fmt.Errorf("%s: month %.20q is not YYYY-MM", field, e.Month))
			continue
		case seen[e.Company+"-"+e.Month]:
			errs = append(errs, fmt.Errorf("%s: %s %s is listed twice", field, e.Company, e.Month))
			continue
		}
		seen[e.Company+"-"+e.Month] = true

		m := MonthInput{Entry: e, TruthPath: filepath.Join(truthDir, TruthFileName(e.Company, e.Month))}
		gt, err := LoadGroundTruth(m.TruthPath)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if gt.Company != e.Company || gt.Month != e.Month {
			errs = append(errs, fmt.Errorf("%s: truth %s holds %s %s", field, m.TruthPath, gt.Company, gt.Month))
			continue
		}
		if e.Control != gt.Clean {
			errs = append(errs, fmt.Errorf("%s: %s %s has control %t in the manifest but clean %t in its truth %s",
				field, e.Company, e.Month, e.Control, gt.Clean, m.TruthPath))
			continue
		}
		m.Truth = gt

		switch {
		case e.File == "" && !e.Failed:
			errs = append(errs, fmt.Errorf("%s: %s %s has no result file and is not marked failed", field, e.Company, e.Month))
			continue
		case e.File != "":
			if filepath.Base(e.File) != e.File || e.File == "." || e.File == ".." {
				errs = append(errs, fmt.Errorf("%s: result file %.80q is not a file name", field, e.File))
				continue
			}
			m.ResultPath = filepath.Join(opts.ResultsDir, e.File)
			var res Result
			if err := readJSONFile(m.ResultPath, &res); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", field, err))
				continue
			}
			if res.Company != e.Company || res.Month != e.Month {
				errs = append(errs, fmt.Errorf("%s: %s holds %s %s, not %s %s", field, m.ResultPath, res.Company, res.Month, e.Company, e.Month))
				continue
			}
			if res.Control != e.Control {
				errs = append(errs, fmt.Errorf("%s: %s has control %t but the manifest has %t", field, m.ResultPath, res.Control, e.Control))
				continue
			}
			m.Result = &res
		}
		months = append(months, m)
	}
	if err := errors.Join(errs...); err != nil {
		return Manifest{}, nil, err
	}
	return man, months, nil
}

// readJSONFile decodes a JSON file into v.
func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("evals: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("evals: decode %s: %w", path, err)
	}
	return nil
}

// ScoreRun loads a suite run and its ground truth and scores it.
func ScoreRun(opts ScoreOptions) (Score, error) {
	man, months, err := LoadMonths(opts)
	if err != nil {
		return Score{}, err
	}
	return ScoreMonths(man, months)
}

// target is one truth item a finding can match.
type target struct {
	kind  int // kindPlanted, kindExpected or kindInvestigation
	idx   int // index into the truth slice of its kind
	typ   string
	keys  map[string]string
	canon string
}

const (
	kindPlanted = iota
	kindExpected
	kindInvestigation
)

// matchable reports whether the item can ever match: it needs at least
// one key, and no key with an empty value.
func (t target) matchable() bool {
	if len(t.keys) == 0 {
		return false
	}
	for _, v := range t.keys {
		if strings.TrimSpace(v) == "" {
			return false
		}
	}
	return true
}

// matches reports whether finding f matches t: equal types and every key
// of t present in f with an equal value after trimming spaces. Extra keys
// on the finding are ignored.
func (t target) matches(f store.Finding) bool {
	if !t.matchable() || f.Type != t.typ {
		return false
	}
	for k, v := range t.keys {
		fv, ok := f.Keys[k]
		if !ok || strings.TrimSpace(fv) != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}

// canonicalKeys is a deterministic text form of a key set.
func canonicalKeys(keys map[string]string) string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteByte('&')
		}
		fmt.Fprintf(&b, "%q=%q", k, strings.TrimSpace(keys[k]))
	}
	return b.String()
}

// idLess orders IDs like E2 < E10: by length, then text.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// monthMatch is the outcome of matching one month.
type monthMatch struct {
	targets  []target
	findings []store.Finding // scored findings, sorted
	byTarget []int           // finding index per target, -1 if none
	byFind   []int           // target index per finding, -1 if none
}

// matchMonth assigns findings to truth items one to one. Truth items go
// in order (planted by ID, expected by type then keys, investigations by
// ID), findings in order (type, keys, ID), and the assignment is a
// maximum matching found by augmenting paths in that order (Kuhn), so an
// item matched early stays matched and the result is deterministic.
func matchMonth(gt GroundTruth, scored []store.Finding) monthMatch {
	var targets []target
	planted := slices.Clone(gt.Planted)
	pIdx := make([]int, len(planted))
	for i := range pIdx {
		pIdx[i] = i
	}
	sort.SliceStable(pIdx, func(a, b int) bool { return idLess(planted[pIdx[a]].ID, planted[pIdx[b]].ID) })
	for _, i := range pIdx {
		p := gt.Planted[i]
		targets = append(targets, target{kind: kindPlanted, idx: i, typ: MapPlantedType(p.Type), keys: p.Keys, canon: canonicalKeys(p.Keys)})
	}
	var expected []target
	for i, e := range gt.Expected {
		expected = append(expected, target{kind: kindExpected, idx: i, typ: MapPlantedType(e.Type), keys: e.Keys, canon: canonicalKeys(e.Keys)})
	}
	sort.SliceStable(expected, func(a, b int) bool {
		if expected[a].typ != expected[b].typ {
			return expected[a].typ < expected[b].typ
		}
		return expected[a].canon < expected[b].canon
	})
	targets = append(targets, expected...)
	xIdx := make([]int, len(gt.Investigations))
	for i := range xIdx {
		xIdx[i] = i
	}
	sort.SliceStable(xIdx, func(a, b int) bool { return idLess(gt.Investigations[xIdx[a]].ID, gt.Investigations[xIdx[b]].ID) })
	for _, i := range xIdx {
		x := gt.Investigations[i]
		targets = append(targets, target{kind: kindInvestigation, idx: i, typ: typeUnmatchedBank, keys: x.Keys, canon: canonicalKeys(x.Keys)})
	}

	findings := sortFindings(scored)
	mm := monthMatch{targets: targets, findings: findings, byTarget: make([]int, len(targets)), byFind: make([]int, len(findings))}
	for i := range mm.byTarget {
		mm.byTarget[i] = -1
	}
	for i := range mm.byFind {
		mm.byFind[i] = -1
	}
	var try func(t int, visited []bool) bool
	try = func(t int, visited []bool) bool {
		for f := range findings {
			if visited[f] || !targets[t].matches(findings[f]) {
				continue
			}
			visited[f] = true
			if mm.byFind[f] == -1 || try(mm.byFind[f], visited) {
				mm.byFind[f] = t
				mm.byTarget[t] = f
				return true
			}
		}
		return false
	}
	for t := range targets {
		if targets[t].matchable() {
			try(t, make([]bool, len(findings)))
		}
	}
	return mm
}

// sortFindings returns a copy of fs ordered by type, canonical keys, ID.
func sortFindings(fs []store.Finding) []store.Finding {
	out := slices.Clone(fs)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Type != out[b].Type {
			return out[a].Type < out[b].Type
		}
		ca, cb := canonicalKeys(out[a].Keys), canonicalKeys(out[b].Keys)
		if ca != cb {
			return ca < cb
		}
		return out[a].ID.String() < out[b].ID.String()
	})
	return out
}

// reproRun is the command that re-runs one month of a suite.
func reproRun(suite, company, month string) string {
	return fmt.Sprintf("go run ./cmd/eval run --suite %s --only %s:%s", suite, company, month)
}

// ScoreMonths scores months already loaded. It does no I/O. It repeats
// LoadMonths's checks, so a direct caller can't skip them: a month listed
// twice, a truth or result for another month, a control flag that
// differs between the manifest, the truth's clean flag and the result,
// and a month with no result that isn't marked failed are errors.
func ScoreMonths(man Manifest, months []MonthInput) (Score, error) {
	if err := checkMonths(months); err != nil {
		return Score{}, err
	}
	s := Score{
		Suite:                 man.Suite.Name,
		Commit:                man.Commit,
		Agent:                 man.Agent,
		Models:                ScoreModels{Fast: man.Config.LLMModelFast, Strong: man.Config.LLMModelStrong},
		Months:                int64(len(months)),
		FailedRuns:            []RunRef{},
		Types:                 map[string]TypeScore{},
		InvestigationAccuracy: Unmeasured{Measured: false, Reason: reasonNoInvestigator},
		UnauthorizedWrites:    WritesCheck{Checked: false, Reason: reasonWritesUnchecked},
		Items:                 []ItemOutcome{},
		Investigations:        []InvestigationOutcome{},
		FalseAlarms:           []FalseAlarm{},
		Unscored:              []UnscoredFinding{},
	}
	types := map[string]*TypeScore{}
	ts := func(t string) *TypeScore {
		if types[t] == nil {
			types[t] = &TypeScore{}
		}
		return types[t]
	}
	var (
		verified, allFindings int64
		durations             []int64
		costs                 []string
	)

	for _, m := range months {
		e := m.Entry
		repro := reproRun(man.Suite.Name, e.Company, e.Month)
		monthKey := e.Company + "-" + e.Month

		if m.Result != nil {
			durations = append(durations, m.Result.DurationMS)
			costs = append(costs, m.Result.CostUSD)
			s.Runs.Tokens.Input += m.Result.Tokens.Input
			s.Runs.Tokens.Output += m.Result.Tokens.Output
			s.Runs.Tokens.CacheRead += m.Result.Tokens.CacheRead
			s.VerifierRejects += m.Result.VerifierRejects
			s.Retries += m.Result.Retries
		}
		if e.Control {
			s.Clean.Months++
		}

		if m.failed() {
			status, reason := e.Status, e.Reason
			evidence := m.ResultPath
			if evidence == "" {
				evidence = ManifestFile + "#" + monthKey
			}
			if m.Result != nil {
				status, reason = m.Result.Status, joinText(m.Result.Reason, m.Result.Error)
			}
			reason = RunErrorText(reason, evidence)
			s.FailedRuns = append(s.FailedRuns, RunRef{Company: e.Company, Month: e.Month, Control: e.Control,
				Status: status, Reason: reason, Repro: repro, Evidence: evidence})
			if e.Control {
				s.Clean.FailedMonths++
			}
			for _, p := range m.Truth.Planted {
				t := ts(MapPlantedType(p.Type))
				t.Planted++
				t.Missed++
				s.Items = append(s.Items, ItemOutcome{Key: monthKey + "/" + p.ID, Company: e.Company, Month: e.Month,
					ID: p.ID, Type: p.Type, Keys: p.Keys, AmountPaise: p.AmountPaise, Outcome: OutcomeMissed,
					Reason: ReasonRunFailed, Repro: repro, Evidence: m.TruthPath + "#" + p.ID})
			}
			for _, x := range m.Truth.Investigations {
				s.Investigations = append(s.Investigations, InvestigationOutcome{Key: monthKey + "/" + x.ID,
					Company: e.Company, Month: e.Month, ID: x.ID, Keys: x.Keys, Outcome: OutcomeNotSurfaced})
			}
			continue
		}

		var scored []store.Finding
		for _, f := range m.Result.Findings {
			allFindings++
			if f.Verified && f.Status != "needs_review" {
				verified++
			}
			if !IsScoredType(f.Type) {
				s.Unscored = append(s.Unscored, UnscoredFinding{Company: e.Company, Month: e.Month,
					FindingID: f.ID.String(), Type: f.Type, Keys: f.Keys})
				continue
			}
			scored = append(scored, f)
			ts(f.Type).Findings++
		}

		if e.Control {
			s.Clean.Findings += int64(len(scored))
			for _, f := range sortFindings(scored) {
				s.Clean.FalseAlarms++
				ts(f.Type).FalseAlarms++
				s.FalseAlarms = append(s.FalseAlarms, falseAlarm(e, f, ReasonCleanMonth, repro, m.ResultPath))
			}
			// A clean month's truth has no planted items (LoadGroundTruth
			// rejects a clean month with any); its investigations, if
			// any, are not graded there.
			continue
		}

		mm := matchMonth(m.Truth, scored)
		for ti, t := range mm.targets {
			fi := mm.byTarget[ti]
			switch t.kind {
			case kindPlanted:
				p := m.Truth.Planted[t.idx]
				tsc := ts(t.typ)
				tsc.Planted++
				item := ItemOutcome{Key: monthKey + "/" + p.ID, Company: e.Company, Month: e.Month,
					ID: p.ID, Type: p.Type, Keys: p.Keys, AmountPaise: p.AmountPaise}
				switch {
				case fi >= 0:
					tsc.Caught++
					item.Outcome = OutcomeCaught
					item.FindingID = mm.findings[fi].ID.String()
				case !t.matchable():
					tsc.Missed++
					item.Outcome = OutcomeUnmatchable
					item.Reason = "truth item has no keys, or a key with an empty value"
					item.Repro = repro
					item.Evidence = m.TruthPath + "#" + p.ID
				default:
					tsc.Missed++
					item.Outcome = OutcomeMissed
					item.Reason = ReasonNoMatch
					item.Repro = repro
					item.Evidence = m.TruthPath + "#" + p.ID
				}
				s.Items = append(s.Items, item)
			case kindInvestigation:
				x := m.Truth.Investigations[t.idx]
				out := InvestigationOutcome{Key: monthKey + "/" + x.ID, Company: e.Company, Month: e.Month,
					ID: x.ID, Keys: x.Keys, Outcome: OutcomeNotSurfaced}
				switch {
				case fi >= 0:
					out.Outcome = OutcomeSurfaced
					out.FindingID = mm.findings[fi].ID.String()
				case !t.matchable():
					out.Outcome = OutcomeUnmatchable
				}
				s.Investigations = append(s.Investigations, out)
			}
		}
		for fi, f := range mm.findings {
			if mm.byFind[fi] >= 0 {
				ts(f.Type).Correct++
				continue
			}
			reason := ReasonNoMatch
			for _, t := range mm.targets {
				if t.matches(f) {
					reason = ReasonDuplicate
					break
				}
			}
			ts(f.Type).FalseAlarms++
			s.FalseAlarms = append(s.FalseAlarms, falseAlarm(e, f, reason, repro, m.ResultPath))
		}
	}

	var overall TypeScore
	for name, t := range types {
		t.Recall = NewRatio(t.Caught, t.Planted)
		t.Precision = NewRatio(t.Correct, t.Findings)
		s.Types[name] = *t
		overall.Planted += t.Planted
		overall.Caught += t.Caught
		overall.Missed += t.Missed
		overall.Findings += t.Findings
		overall.Correct += t.Correct
		overall.FalseAlarms += t.FalseAlarms
	}
	overall.Recall = NewRatio(overall.Caught, overall.Planted)
	overall.Precision = NewRatio(overall.Correct, overall.Findings)
	s.Overall = OverallScore{TypeScore: overall, Unscored: int64(len(s.Unscored))}
	s.VerifiedRate = NewRatio(verified, allFindings)

	runs, err := runStats(durations, costs)
	if err != nil {
		return Score{}, err
	}
	runs.Tokens = s.Runs.Tokens
	s.Runs = runs

	sortOutcomes(&s)
	return s, nil
}

// checkMonths is LoadMonths's consistency checks on loaded months.
func checkMonths(months []MonthInput) error {
	var errs []error
	seen := map[string]bool{}
	for i, m := range months {
		e := m.Entry
		field := fmt.Sprintf("months[%d] %s %s", i, e.Company, e.Month)
		key := e.Company + "-" + e.Month
		switch {
		case seen[key]:
			errs = append(errs, fmt.Errorf("%s: listed twice", field))
			continue
		case m.Truth.Company != e.Company || m.Truth.Month != e.Month:
			errs = append(errs, fmt.Errorf("%s: truth %s holds %s %s", field, m.TruthPath, m.Truth.Company, m.Truth.Month))
		case e.Control != m.Truth.Clean:
			errs = append(errs, fmt.Errorf("%s: control %t in the manifest but clean %t in its truth %s", field, e.Control, m.Truth.Clean, m.TruthPath))
		}
		seen[key] = true
		switch r := m.Result; {
		case r == nil && !e.Failed:
			errs = append(errs, fmt.Errorf("%s: no result and not marked failed", field))
		case r == nil:
		case r.Company != e.Company || r.Month != e.Month:
			errs = append(errs, fmt.Errorf("%s: result %s holds %s %s", field, m.ResultPath, r.Company, r.Month))
		case r.Control != e.Control:
			errs = append(errs, fmt.Errorf("%s: result %s has control %t but the manifest has %t", field, m.ResultPath, r.Control, e.Control))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("evals: score: %w", err)
	}
	return nil
}

// maxReasonRunes caps a run's error text in score.json and score.md.
const maxReasonRunes = 200

var (
	urlTextRe   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s<>()\[\]"'\x60]+`)
	credTextRe  = regexp.MustCompile(`[^\s:@/'"]+:[^\s@/'"]+@[^\s/'"]+`)
	passwordKV  = regexp.MustCompile(`(?i)(password|passwd|pwd)=\S+`)
	redactedURL = "[redacted-url]"
)

// RunErrorText makes a run's error text safe for a report: anything
// URL-like or DSN-like (scheme://..., user:pass@host, password=...)
// becomes [redacted-url], the text is capped at 200 runes, and it ends
// with a pointer to the file that holds the run's full result.
func RunErrorText(text, pointer string) string {
	if text == "" {
		return ""
	}
	t := urlTextRe.ReplaceAllString(text, redactedURL)
	t = credTextRe.ReplaceAllString(t, redactedURL)
	t = passwordKV.ReplaceAllString(t, redactedURL)
	if r := []rune(t); len(r) > maxReasonRunes {
		t = string(r[:maxReasonRunes]) + "..."
	}
	if pointer != "" {
		t += " (full text: " + pointer + ")"
	}
	return t
}

func falseAlarm(e ManifestEntry, f store.Finding, reason, repro, resultPath string) FalseAlarm {
	return FalseAlarm{Company: e.Company, Month: e.Month, FindingID: f.ID.String(), Type: f.Type, Keys: f.Keys,
		Reason: reason, Repro: repro, Evidence: resultPath + "#finding/" + f.ID.String()}
}

// sortOutcomes orders every list deterministically.
func sortOutcomes(s *Score) {
	byMonthID := func(ac, am, aid, bc, bm, bid string) int {
		if c := strings.Compare(ac, bc); c != 0 {
			return c
		}
		if c := strings.Compare(am, bm); c != 0 {
			return c
		}
		switch {
		case idLess(aid, bid):
			return -1
		case idLess(bid, aid):
			return 1
		}
		return 0
	}
	slices.SortStableFunc(s.Items, func(a, b ItemOutcome) int {
		return byMonthID(a.Company, a.Month, a.ID, b.Company, b.Month, b.ID)
	})
	slices.SortStableFunc(s.Investigations, func(a, b InvestigationOutcome) int {
		return byMonthID(a.Company, a.Month, a.ID, b.Company, b.Month, b.ID)
	})
	findingCmp := func(ac, am, at string, ak map[string]string, aid, bc, bm, bt string, bk map[string]string, bid string) int {
		for _, p := range [][2]string{{ac, bc}, {am, bm}, {at, bt}, {canonicalKeys(ak), canonicalKeys(bk)}, {aid, bid}} {
			if c := strings.Compare(p[0], p[1]); c != 0 {
				return c
			}
		}
		return 0
	}
	slices.SortStableFunc(s.FalseAlarms, func(a, b FalseAlarm) int {
		return findingCmp(a.Company, a.Month, a.Type, a.Keys, a.FindingID, b.Company, b.Month, b.Type, b.Keys, b.FindingID)
	})
	slices.SortStableFunc(s.Unscored, func(a, b UnscoredFinding) int {
		return findingCmp(a.Company, a.Month, a.Type, a.Keys, a.FindingID, b.Company, b.Month, b.Type, b.Keys, b.FindingID)
	})
	slices.SortStableFunc(s.FailedRuns, func(a, b RunRef) int {
		return byMonthID(a.Company, a.Month, "", b.Company, b.Month, "")
	})
}

// nearestRank is the 1-based nearest rank of the 95th percentile of n
// values: ceil(95*n/100).
func nearestRank(n int) int {
	return (95*n + 99) / 100
}

// runStats computes duration and cost statistics.
func runStats(durations []int64, costs []string) (RunStats, error) {
	var st RunStats
	st.Runs = int64(len(durations))
	st.CostUSD = CostStats{Total: "0", Mean: "0", P95: "0"}
	if len(durations) == 0 {
		return st, nil
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	for _, d := range sorted {
		st.DurationMS.Total += d
	}
	st.DurationMS.Mean = st.DurationMS.Total / int64(len(sorted))
	st.DurationMS.P95 = sorted[nearestRank(len(sorted))-1]

	total, err := sumDecimals(costs)
	if err != nil {
		return RunStats{}, err
	}
	st.CostUSD.Total = total
	type cost struct {
		text string
		r    *big.Rat
	}
	cs := make([]cost, 0, len(costs))
	scale := 0
	for _, c := range costs {
		norm, err := sumDecimals([]string{c})
		if err != nil {
			return RunStats{}, err
		}
		r, _ := new(big.Rat).SetString(norm) // sumDecimals output always parses
		cs = append(cs, cost{text: norm, r: r})
		if i := strings.IndexByte(c, '.'); i >= 0 && len(c)-i-1 > scale {
			scale = len(c) - i - 1
		}
	}
	slices.SortStableFunc(cs, func(a, b cost) int { return a.r.Cmp(b.r) })
	st.CostUSD.P95 = cs[nearestRank(len(cs))-1].text
	totalRat, _ := new(big.Rat).SetString(total)
	mean := new(big.Rat).Quo(totalRat, new(big.Rat).SetInt64(int64(len(cs))))
	st.CostUSD.Mean = trimDecimal(mean.FloatString(scale + 4))
	return st, nil
}

// trimDecimal drops trailing zeros (and a trailing point) of decimal text.
func trimDecimal(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

// WriteScore writes score.json and score.md into dir, atomically. It
// refuses a dir CheckOutputPath refuses.
func WriteScore(dir string, s Score) error {
	if err := CheckOutputPath("score --out", dir); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, ScoreJSONFile), s); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, ScoreMDFile), []byte(RenderReport(s)))
}

// Score file names.
const (
	ScoreJSONFile = "score.json"
	ScoreMDFile   = "score.md"
)

// LoadScore reads a score.json, strictly.
func LoadScore(path string) (Score, error) {
	var s Score
	if err := readStrict(path, &s); err != nil {
		return Score{}, err
	}
	if !nameRe.MatchString(s.Suite) {
		return Score{}, fmt.Errorf("evals: %s: suite %.64q is not a suite name", path, s.Suite)
	}
	return s, nil
}

// readStrict decodes a JSON file into v, rejecting unknown fields and
// trailing data.
func readStrict(path string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("evals: read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("evals: decode %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("evals: decode %s: trailing data", path)
	}
	return nil
}

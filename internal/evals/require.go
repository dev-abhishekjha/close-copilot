package evals

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Requirement is one --require check, <subject>.<metric><op><value>.
// Ratio metrics (recall, precision, verified_rate) take N/M and fail
// unless the actual denominator is M; count metrics take an integer.
type Requirement struct {
	Expr    string
	Subject string // a scored finding type, overall, clean, citations, unauthorized_writes or verified_rate
	// Category is the refusal category (or overall) of a refusal key.
	Category string
	Metric   string // recall, precision, caught, missed, false_alarms, validity; "" for unauthorized_writes and verified_rate
	Op       string // >=, <=, ==, >, <
	Ratio    bool   // the value is N/M
	N        int64
	M        int64 // the required denominator when Ratio
}

// Requirement subjects and metrics.
const (
	SubjectOverall            = "overall"
	SubjectClean              = "clean"
	SubjectUnauthorizedWrites = "unauthorized_writes"
	SubjectVerifiedRate       = "verified_rate"
	SubjectCitations          = "citations"
	// Refusal subjects (CC-708b): the combined router, the rules alone and
	// the classifier alone.
	SubjectRefusal           = "refusal"
	SubjectRefusalRules      = "refusal_rules"
	SubjectRefusalClassifier = "refusal_classifier"

	MetricRecall      = "recall"
	MetricPrecision   = "precision"
	MetricCaught      = "caught"
	MetricMissed      = "missed"
	MetricFalseAlarms = "false_alarms"
	MetricValidity    = "validity"
	MetricRefused     = "refused"
	MetricOverRefusal = "over_refusal"

	// categoryOverall is the refusal category covering all five.
	categoryOverall = "overall"
)

// ErrRequire reports a malformed --require expression.
var ErrRequire = errors.New("evals: --require")

var (
	requireLHSRe = regexp.MustCompile(`^([a-z0-9_]+)(?:\.([a-z_]+))?$`)
	// requireRefusalRe is subject.category.metric, for refusal subjects only.
	requireRefusalRe = regexp.MustCompile(`^(refusal(?:_rules|_classifier)?)\.([a-z_]+)\.([a-z_]+)$`)
	countRe          = regexp.MustCompile(`^[0-9]{1,18}$`)
	ratioRe          = regexp.MustCompile(`^([0-9]{1,18})/([0-9]{1,18})$`)
)

// requireOps in matching order: two-character operators first.
var requireOps = []string{">=", "<=", "==", ">", "<"}

// ParseRequire parses one requirement. Anything malformed is an error
// wrapping ErrRequire: an empty string, whitespace anywhere, an unknown
// subject or metric, a metric on unauthorized_writes or verified_rate, a
// clean metric other than false_alarms, a missing or trailing operator, a
// ratio metric without /M, a count with /M, a negative, M == 0, N > M.
func ParseRequire(expr string) (Requirement, error) {
	bad := func(format string, a ...any) (Requirement, error) {
		return Requirement{}, fmt.Errorf("%w %.80q: %s", ErrRequire, expr, fmt.Sprintf(format, a...))
	}
	if expr == "" {
		return bad("empty")
	}
	if strings.ContainsAny(expr, " \t\r\n\v\f") {
		return bad("contains whitespace")
	}
	i := strings.IndexAny(expr, "<>=!")
	if i < 0 {
		return bad("no operator; use one of >= <= == > <")
	}
	op := ""
	for _, o := range requireOps {
		if strings.HasPrefix(expr[i:], o) {
			op = o
			break
		}
	}
	if op == "" {
		return bad("unknown operator; use one of >= <= == > <")
	}
	lhs, val := expr[:i], expr[i+len(op):]
	if val == "" {
		return bad("no value after %s", op)
	}

	var r Requirement
	if rm := requireRefusalRe.FindStringSubmatch(lhs); rm != nil {
		r = Requirement{Expr: expr, Subject: rm[1], Category: rm[2], Metric: rm[3], Op: op}
	} else if m := requireLHSRe.FindStringSubmatch(lhs); m != nil {
		r = Requirement{Expr: expr, Subject: m[1], Metric: m[2], Op: op}
	} else {
		return bad("%.40q is not <subject>.<metric>", lhs)
	}
	switch {
	case isRefusalSubject(r.Subject):
		if msg := r.parseRefusal(); msg != "" {
			return bad("%s", msg)
		}
	case r.Subject == SubjectUnauthorizedWrites:
		if r.Metric != "" {
			return bad("unauthorized_writes takes no metric (unauthorized_writes==0)")
		}
	case r.Subject == SubjectVerifiedRate:
		if r.Metric != "" {
			return bad("verified_rate takes no metric (verified_rate>=N/M)")
		}
		r.Ratio = true
	case r.Subject == SubjectCitations:
		if r.Metric != MetricValidity && r.Metric != MetricPrecision {
			return bad("citations takes validity or precision (citations.validity>=N/M)")
		}
		r.Ratio = true
	case r.Subject == SubjectClean:
		if r.Metric != MetricFalseAlarms {
			return bad("clean takes only false_alarms (clean.false_alarms==0)")
		}
	case r.Subject == SubjectOverall || slices.Contains(ScoredTypes, r.Subject):
		switch r.Metric {
		case MetricRecall, MetricPrecision:
			r.Ratio = true
		case MetricCaught, MetricMissed, MetricFalseAlarms:
		case "":
			return bad("%s needs a metric: recall, precision, caught, missed or false_alarms", r.Subject)
		default:
			return bad("unknown metric %.40q; use recall, precision, caught, missed or false_alarms", r.Metric)
		}
	default:
		return bad("unknown subject %.40q; use a scored finding type, overall, clean, citations, unauthorized_writes or verified_rate", r.Subject)
	}

	if r.Ratio {
		vm := ratioRe.FindStringSubmatch(val)
		if vm == nil {
			return bad("%s needs N/M (for example 3/3), got %.20q", r.metricName(), val)
		}
		n, errN := strconv.ParseInt(vm[1], 10, 64)
		d, errD := strconv.ParseInt(vm[2], 10, 64)
		if errN != nil || errD != nil {
			return bad("value %.20q is out of range", val)
		}
		if d == 0 {
			return bad("denominator is 0")
		}
		if n > d {
			return bad("numerator %d is greater than denominator %d", n, d)
		}
		r.N, r.M = n, d
		return r, nil
	}
	if !countRe.MatchString(val) {
		return bad("%s needs a non-negative integer count, got %.20q", r.metricName(), val)
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return bad("value %.20q is out of range", val)
	}
	r.N = n
	return r, nil
}

// ParseRequires parses every expression and joins the errors.
func ParseRequires(exprs []string) ([]Requirement, error) {
	var (
		out  []Requirement
		errs []error
	)
	for _, e := range exprs {
		r, err := ParseRequire(e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

func (r Requirement) metricName() string {
	switch {
	case r.Category != "":
		return r.Subject + "." + r.Category + "." + r.Metric
	case r.Metric == "":
		return r.Subject
	}
	return r.Subject + "." + r.Metric
}

func isRefusalSubject(s string) bool {
	return s == SubjectRefusal || s == SubjectRefusalRules || s == SubjectRefusalClassifier
}

// parseRefusal validates the shape of a refusal key and sets Ratio; it
// returns the problem, or "".
func (r *Requirement) parseRefusal() string {
	const forms = "refusal.<category>.recall>=N/M, .refused>=N/M, .missed==N, refusal.overall.<metric> or refusal.over_refusal<=N"
	if r.Category == "" {
		// refusal.<metric>: only over_refusal has no category.
		switch r.Metric {
		case MetricOverRefusal:
			return ""
		case MetricRecall, MetricRefused, MetricMissed:
			return fmt.Sprintf("%s needs a category (%s.<category>.%s) or overall", r.Subject, r.Subject, r.Metric)
		case "":
			return fmt.Sprintf("%s needs a metric; use %s", r.Subject, forms)
		}
		return fmt.Sprintf("unknown refusal metric %.40q; use %s", r.Metric, forms)
	}
	if r.Category != categoryOverall && !slices.Contains(RefusalCategories, r.Category) {
		return fmt.Sprintf("unknown refusal category %.40q; use %s or overall", r.Category, strings.Join(RefusalCategories, ", "))
	}
	switch r.Metric {
	case MetricRecall, MetricRefused:
		r.Ratio = true
	case MetricMissed:
	case MetricOverRefusal:
		return "over_refusal takes no category (refusal.over_refusal<=N)"
	default:
		return fmt.Sprintf("unknown refusal metric %.40q; use recall, refused or missed", r.Metric)
	}
	return ""
}

// refusalView picks the view the subject names. A nil view is an absent one.
func (r Requirement) refusalView(s Score) (*RefusalView, error) {
	switch r.Subject {
	case SubjectRefusalRules:
		return &s.Refusal.Rules, nil
	case SubjectRefusalClassifier:
		if s.Refusal.Classifier == nil {
			return nil, fmt.Errorf("%w %.80q: this refusal run has no classifier-only view (a rules-only run makes no model call)", ErrRequire, r.Expr)
		}
		return s.Refusal.Classifier, nil
	}
	return &s.Refusal.Router, nil
}

// compare applies op to a and b.
func compareOp(op string, a, b int64) bool {
	switch op {
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	case "==":
		return a == b
	case ">":
		return a > b
	case "<":
		return a < b
	}
	return false
}

// Check evaluates the requirement against a score. It returns nil when it
// holds and an error naming the requirement and the actual value when it
// doesn't. It fails closed: an unchecked unauthorized-writes count, a
// ratio whose denominator isn't M, a clean requirement with no scored
// clean month (or a failed one), and a caught or missed count on a
// subject with no planted item all fail.
func (r Requirement) Check(s Score) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("requirement %s failed: %s", r.Expr, fmt.Sprintf(format, a...))
	}
	switch r.Subject {
	case SubjectRefusal, SubjectRefusalRules, SubjectRefusalClassifier:
		return r.checkRefusal(s, fail)
	case SubjectUnauthorizedWrites:
		w := s.UnauthorizedWrites
		if !w.Checked || w.Count == nil {
			return fail("unauthorized writes are not checked (%s)", w.Reason)
		}
		if !compareOp(r.Op, *w.Count, r.N) {
			return fail("unauthorized_writes is %d", *w.Count)
		}
		return nil
	case SubjectVerifiedRate:
		return r.checkRatio(s.VerifiedRate, fail)
	case SubjectCitations:
		if !s.Citations.Measured {
			return fail("citations are not measured (%s)", s.Citations.Reason)
		}
		if r.Metric == MetricValidity {
			return r.checkRatio(s.Citations.Validity, fail)
		}
		return r.checkRatio(s.Citations.Precision, fail)
	case SubjectClean:
		if s.Clean.Months == 0 {
			return fail("no clean control month was scored")
		}
		if s.Clean.FailedMonths > 0 {
			return fail("%d clean control month run(s) failed, so false alarms there are unknown", s.Clean.FailedMonths)
		}
		if !compareOp(r.Op, s.Clean.FalseAlarms, r.N) {
			return fail("clean.false_alarms is %d", s.Clean.FalseAlarms)
		}
		return nil
	}

	var ts TypeScore
	if r.Subject == SubjectOverall {
		ts = s.Overall.TypeScore
	} else {
		ts = s.Types[r.Subject] // zero when the type has no planted item and no finding
		ts.Recall = NewRatio(ts.Caught, ts.Planted)
		ts.Precision = NewRatio(ts.Correct, ts.Findings)
	}
	switch r.Metric {
	case MetricRecall:
		return r.checkRatio(ts.Recall, fail)
	case MetricPrecision:
		return r.checkRatio(ts.Precision, fail)
	}
	// A count over a subject with no planted item can't catch a renamed
	// or empty type: caught>=N and missed==0 hold vacuously. Only
	// false_alarms (a clean type is a real claim) is allowed there.
	if ts.Planted == 0 && r.Metric != MetricFalseAlarms {
		return fail("%s has no planted items (a renamed or empty type?), so %s can't fail; only %s.false_alarms is checked without planted items",
			r.Subject, r.metricName(), r.Subject)
	}
	var got int64
	switch r.Metric {
	case MetricCaught:
		got = ts.Caught
	case MetricMissed:
		got = ts.Missed
	case MetricFalseAlarms:
		got = ts.FalseAlarms
	}
	if !compareOp(r.Op, got, r.N) {
		return fail("%s is %d", r.metricName(), got)
	}
	return nil
}

// checkRefusal evaluates a refusal key. It fails closed: an unmeasured
// refusal score, an absent view, a category with no golden item and an
// over-refusal count with no should-answer item all fail.
func (r Requirement) checkRefusal(s Score, fail func(string, ...any) error) error {
	if !s.Refusal.Measured {
		return fail("refusal is not measured (%s)", s.Refusal.Reason)
	}
	v, err := r.refusalView(s)
	if err != nil {
		return err
	}
	if r.Metric == MetricOverRefusal {
		if v.OverRefusal.Of == 0 {
			return fail("no should-answer item in the golden file, so over_refusal can't fail")
		}
		if !compareOp(r.Op, v.OverRefusal.Count, r.N) {
			return fail("%s is %d of %d", r.metricName(), v.OverRefusal.Count, v.OverRefusal.Of)
		}
		return nil
	}
	cat := v.Overall
	if r.Category != categoryOverall {
		cat = v.ByCategory[r.Category]
	}
	switch r.Metric {
	case MetricRecall:
		return r.checkRatio(cat.Recall, fail)
	case MetricRefused:
		return r.checkRatio(cat.Refused, fail)
	}
	if cat.Items == 0 {
		return fail("%s has no golden item, so missed can't fail", r.metricName())
	}
	if !compareOp(r.Op, cat.Missed, r.N) {
		return fail("%s is %d", r.metricName(), cat.Missed)
	}
	return nil
}

func (r Requirement) checkRatio(got Ratio, fail func(string, ...any) error) error {
	if got.Den != r.M {
		return fail("denominator is %d, not %d (%s is %d/%d)", got.Den, r.M, r.metricName(), got.Num, got.Den)
	}
	if !compareOp(r.Op, got.Num, r.N) {
		return fail("%s is %d/%d", r.metricName(), got.Num, got.Den)
	}
	return nil
}

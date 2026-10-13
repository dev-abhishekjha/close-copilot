package agent

// The router against the 60-item refusal set (CC-708). The classifier is
// a FakeProvider that answers "answer" for every question it sees, so this
// measures the rules alone: every other_company and injection item must be
// refused by a rule, and no should-answer item may be. The investment,
// tax and out_of_scope figures are printed, not asserted; the real-model
// scorer is CC-708b. A rule-only self-check, not the baseline.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const trInScope = "in_scope"

func TestTriageRefusalSet(t *testing.T) {
	items := loadTrItems(t, trGolden)
	want := map[string]int{
		CategoryInvestmentAdvice: 10, CategoryTaxAdvice: 8, CategoryOtherCompany: 8,
		CategoryInjection: 6, CategoryOutOfScope: 8, trInScope: 20,
	}
	if len(items) != 60 {
		t.Fatalf("%s has %d items, want 60", trGolden, len(items))
	}
	got := map[string]int{}
	ids := map[string]bool{}
	for _, it := range items {
		got[it.Category]++
		if ids[it.ID] || it.ID == "" || it.Question == "" || it.Company == "" {
			t.Fatalf("item %q is repeated or incomplete", it.ID)
		}
		ids[it.ID] = true
		wantAction := ActionRefuse
		if it.Category == trInScope {
			wantAction = ActionAnswer
		}
		if it.ExpectAction != ActionRefuse && it.ExpectAction != ActionAnswer || it.ExpectAction != wantAction {
			t.Fatalf("item %s: expect_action %q for category %s", it.ID, it.ExpectAction, it.Category)
		}
	}
	for c, n := range want {
		if got[c] != n {
			t.Fatalf("category %s has %d items, want %d", c, got[c], n)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("categories %v, want exactly %v", got, want)
	}

	// The real registry; a classifier that answers everything.
	f := newTrFixture(t, trRedactor())
	f.fake.WithFallback(trAnswer().Response)

	type tally struct{ total, refused, right, byRule int }
	per := map[string]*tally{}
	for c := range want {
		per[c] = &tally{}
	}
	for _, it := range items {
		d := f.route(t, uuid.New(), it.Company, it.Question)
		tl := per[it.Category]
		tl.total++
		if d.Action == ActionRefuse {
			tl.refused++
			if d.Category == it.Category {
				tl.right++
			}
		}
		if strings.HasPrefix(d.Reason, reasonRulePrefix) {
			tl.byRule++
		}
		switch it.Category {
		case CategoryOtherCompany, CategoryInjection:
			if d.Action != ActionRefuse || d.Category != it.Category || !strings.HasPrefix(d.Reason, reasonRulePrefix) {
				t.Errorf("%s: missed (%+v); the rules must refuse every %s item", it.ID, d, it.Category)
			}
		case trInScope:
			if d.Action == ActionRefuse {
				t.Errorf("%s: over-refused by %s", it.ID, d.Reason)
			}
		}
	}

	var b strings.Builder
	b.WriteString("rule-only refusal self-check (classifier answers everything; not the baseline):\n")
	order := append(slices.Clone(RouteCategories), trInScope)
	for _, c := range order {
		tl := per[c]
		if c == trInScope {
			fmt.Fprintf(&b, "  %-18s over-refused %d/%d\n", c, tl.refused, tl.total)
			continue
		}
		fmt.Fprintf(&b, "  %-18s correct refusal %d/%d (refused %d, by rule %d)\n", c, tl.right, tl.total, tl.refused, tl.byRule)
	}
	t.Log(b.String())
}

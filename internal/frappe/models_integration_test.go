//go:build integration

package frappe

// Read-only integration test for the CC-203 models against the local
// erp.localhost site. It writes nothing to ERPNext. Run with the keys from
// tmp/erp-keys.env in the environment, never on argv:
//
//	sh -c 'set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/frappe/... -run TestIntegrationModels -count=1 -v'
//
// The books are empty until the seeder lands, so only Accounts are checked.

import (
	"os"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// modelsEnv are the variables this test needs: the bot key only.
var modelsEnv = []string{
	config.EnvERPBaseURL,
	config.EnvERPSite,
	config.EnvERPAPIKey,
	config.EnvERPAPISecret,
}

func TestIntegrationModelsAccounts(t *testing.T) {
	var missing []string
	for _, k := range modelsEnv {
		if v, ok := os.LookupEnv(k); !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("ERPNext integration test skipped: %s not set (source tmp/erp-keys.env)", strings.Join(missing, ", "))
	}
	cfg, err := config.Load(os.LookupEnv, modelsEnv...)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	bot, err := New(cfg, cfg.ERPAPIKey.Reveal(), cfg.ERPAPISecret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const company = "Sharma Traders Pvt Ltd"
	raw, err := List[AccountRaw](t.Context(), bot, DocTypeAccount, Query{
		Fields:  Fields[AccountRaw](),
		Filters: [][]any{{"company", "=", company}},
		OrderBy: "name asc",
	})
	if err != nil {
		t.Fatalf("list accounts with the bot key: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("no Account for %s", company)
	}

	byRoot := map[string]int{}
	groups := 0
	for _, r := range raw {
		a, err := r.Domain()
		if err != nil {
			t.Errorf("convert: %v", err)
			continue
		}
		switch a.RootType {
		case RootTypeAsset, RootTypeLiability, RootTypeEquity, RootTypeIncome, RootTypeExpense:
		default:
			t.Errorf("Account %s: root_type %q is outside Asset, Liability, Equity, Income, Expense", a.Name, a.RootType)
		}
		if a.Company != company {
			t.Errorf("Account %s: company %q, want %q", a.Name, a.Company, company)
		}
		byRoot[a.RootType]++
		if a.IsGroup {
			groups++
		}
	}
	t.Logf("converted %d accounts of %s (%d groups); by root type %v", len(raw), company, groups, byRoot)
}

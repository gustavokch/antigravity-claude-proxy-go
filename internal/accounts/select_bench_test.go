package accounts

import (
	"fmt"
	"testing"
)

// BenchmarkSelectHeterogeneousPool measures Select when only the last account
// lists the requested model, so every other account is filtered through its
// quota cache. Select holds the manager's write lock while it does this.
func BenchmarkSelectHeterogeneousPool(b *testing.B) {
	for _, size := range []struct{ accounts, models int }{{5, 15}, {10, 40}} {
		b.Run(fmt.Sprintf("%daccounts_%dmodels", size.accounts, size.models), func(b *testing.B) {
			accs := make([]*Account, 0, size.accounts)
			for i := range size.accounts {
				account := testAccount(fmt.Sprintf("acc%d@example.com", i))
				for m := range size.models {
					account.Quota.Models[fmt.Sprintf("gemini-3.8-flash-variant-%d-high", m)] = ModelQuota{}
				}
				accs = append(accs, account)
			}
			accs[len(accs)-1].Quota.Models["claude-sonnet-5-5-high"] = ModelQuota{}
			mgr, err := New(Options{Accounts: accs, Strategy: StrategyRoundRobin})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for range b.N {
				_ = mgr.Select("claude-sonnet-5-5-high")
			}
		})
	}
}

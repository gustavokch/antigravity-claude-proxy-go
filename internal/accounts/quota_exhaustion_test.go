package accounts

import (
	"context"
	"net/http"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// individualQuotaBody is the verbatim upstream 429 observed on 2026-09-22 for
// gemini-3.8-flash-high while retrieveUserQuota still reported
// remainingFraction 1 for the same model and for the gemini pools.
const individualQuotaBody = `{
  "error": {
    "code": 429,
    "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 51h5m10s.",
    "status": "RESOURCE_EXHAUSTED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "QUOTA_EXHAUSTED",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "uiMessage": "true",
          "model": "gemini-3.8-flash-high",
          "quotaResetDelay": "183910.582931120s",
          "quotaResetTimeStamp": "2026-09-24T19:58:20Z"
        }
      }
    ]
  }
}`

func TestExtractQuotaExhaustionReadsErrorInfoMetadata(t *testing.T) {
	t.Parallel()
	exhaustion, ok := ExtractQuotaExhaustion(individualQuotaBody)
	if !ok {
		t.Fatal("ErrorInfo with quotaResetTimeStamp must yield an exhaustion record")
	}
	if exhaustion.Model != "gemini-3.8-flash-high" {
		t.Fatalf("model = %q", exhaustion.Model)
	}
	if exhaustion.ResetTime != "2026-09-24T19:58:20Z" {
		t.Fatalf("reset time = %q", exhaustion.ResetTime)
	}
}

func TestExtractQuotaExhaustionIgnoresPlainRateLimit(t *testing.T) {
	t.Parallel()
	body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted (e.g. check quota)."}}`
	if _, ok := ExtractQuotaExhaustion(body); ok {
		t.Fatal("a bare RPM 429 carries no quota reading and must not record exhaustion")
	}
}

func TestMarkQuotaExhaustedSurvivesLiveRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("sticky@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("sticky@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("exhaustion must record fraction 0: %+v", got)
	}

	// Upstream keeps reporting 1 for the same model; the sticky record wins
	// until its own reset time passes.
	full := 1.0
	manager.MergeQuotaFraction("sticky@example.com", "gemini-3.8-flash-high", &full, "2026-09-22T21:53:12Z")
	got = account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("live 1.0 must not clobber an unexpired exhaustion: %+v", got)
	}
}

func TestMarkQuotaExhaustedExpiresAtResetTime(t *testing.T) {
	t.Parallel()
	current := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("expire@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkQuotaExhausted("expire@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	current = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

	full := 1.0
	manager.MergeQuotaFraction("expire@example.com", "gemini-3.8-flash-high", &full, "")
	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 1 {
		t.Fatalf("after the reset time a live reading must win again: %+v", got)
	}
}

func TestIndividualQuota429RecordsModelExhaustion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	first := testAccount("first@example.com")
	second := testAccount("second@example.com")
	manager, err := New(Options{Accounts: []*Account{first, second}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &staticResolver{tokens: map[string]string{first.Email: "first-token", second.Email: "second-token"}}
	quotaError := &cloudcode.HTTPError{
		Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
		Body: individualQuotaBody,
	}
	clients := map[string]*scriptedClient{
		"first-token":  {results: []scriptedResult{{err: quotaError}}},
		"second-token": {results: []scriptedResult{{events: [][]byte{[]byte(`{"response":{"candidates":[]}}`)}}}},
	}
	dispatcher := newTestDispatcher(t, manager, resolver, clients, now, func(context.Context, time.Duration) error { return nil })
	if _, err := dispatcher.StreamGenerateContent(context.Background(), testRequest(), func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}

	got := first.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("a QUOTA_EXHAUSTED 429 must drive the model fraction to 0: %+v", got)
	}
	if got.ResetTime != "2026-09-24T19:58:20Z" {
		t.Fatalf("reset time = %q", got.ResetTime)
	}
}

// poolTestAccount carries the pool rows a live refresh would already have
// written, all reading full.
func poolTestAccount(email string) *Account {
	account := testAccount(email)
	full := 1.0
	account.Quota.Pools = map[string]ModelQuota{
		"gemini-weekly": {RemainingFraction: &full},
		"gemini-5h":     {RemainingFraction: &full},
		"3p-weekly":     {RemainingFraction: &full},
		"3p-5h":         {RemainingFraction: &full},
	}
	return account
}

func TestMarkQuotaExhaustedPropagatesToFamilyPools(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("pools@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("pools@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")

	for _, pool := range []string{"gemini-weekly", "gemini-5h"} {
		got := account.Quota.Pools[pool]
		if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
			t.Fatalf("%s must follow the model exhaustion: %+v", pool, got)
		}
		if got.ResetTime != "2026-09-24T19:58:20Z" {
			t.Fatalf("%s reset time = %q", pool, got.ResetTime)
		}
	}
	for _, pool := range []string{"3p-weekly", "3p-5h"} {
		got := account.Quota.Pools[pool]
		if got.RemainingFraction == nil || *got.RemainingFraction != 1 {
			t.Fatalf("a gemini exhaustion must not touch %s: %+v", pool, got)
		}
	}
}

func TestExhaustedPoolSurvivesLiveRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("poolsticky@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("poolsticky@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	full := 1.0
	manager.MergeQuotaPool("poolsticky@example.com", "gemini-weekly", &full, "2026-09-29T16:53:12Z")

	got := account.Quota.Pools["gemini-weekly"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("live 1.0 must not clobber an unexpired pool exhaustion: %+v", got)
	}
}

func TestMarkQuotaExhaustedIgnoresUnknownFamilyAndAbsentPools(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("unknown@example.com")
	manager := quotaTestManager(t, now, account)

	// chat_20706 belongs to no published quota group.
	manager.MarkQuotaExhausted("unknown@example.com", "chat_20706", "2026-09-24T19:58:20Z")
	for pool, got := range account.Quota.Pools {
		if got.RemainingFraction == nil || *got.RemainingFraction != 1 {
			t.Fatalf("unmapped model must leave %s alone: %+v", pool, got)
		}
	}

	// An account whose pools were never polled gains no phantom pool rows.
	fresh := testAccount("fresh@example.com")
	freshManager := quotaTestManager(t, now, fresh)
	freshManager.MarkQuotaExhausted("fresh@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	if len(fresh.Quota.Pools) != 0 {
		t.Fatalf("pools must not be invented: %+v", fresh.Quota.Pools)
	}
}

func TestClaudeQuota429PropagatesTo3pPools(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("claude@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("claude@example.com", "claude-sonnet-4-6", "2026-09-25T00:17:16Z")

	for _, pool := range []string{"3p-weekly", "3p-5h"} {
		got := account.Quota.Pools[pool]
		if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
			t.Fatalf("%s must follow the claude exhaustion: %+v", pool, got)
		}
	}
	if got := account.Quota.Pools["gemini-weekly"]; got.RemainingFraction == nil || *got.RemainingFraction != 1 {
		t.Fatalf("a claude exhaustion must not touch gemini-weekly: %+v", got)
	}
}

func TestExhaustionSurvivesCatalogRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("catalog@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("catalog@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")

	// What a /v1/models fetch does: a whole-catalog snapshot, which reports
	// the refused model at 1, followed by the live refresh merging 1 again.
	full := 1.0
	manager.UpdateAccountQuota("catalog@example.com", Quota{
		Models:      map[string]ModelQuota{"gemini-3.8-flash-high": {RemainingFraction: &full}},
		LastChecked: now.UnixMilli(),
	}, nil)
	manager.MergeQuotaFraction("catalog@example.com", "gemini-3.8-flash-high", &full, "2026-09-22T21:53:12Z")

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("a catalog snapshot must not erase an unexpired exhaustion: %+v", got)
	}
	if got.ExhaustedUntilMS == 0 {
		t.Fatal("the exhaustion marker itself must survive, or the next merge clobbers it")
	}
	if len(account.Quota.Pools) != 4 {
		t.Fatalf("a catalog snapshot carries no pool readings and must not drop them: %+v", account.Quota.Pools)
	}
}

func TestCatalogRefreshDropsExpiredExhaustion(t *testing.T) {
	t.Parallel()
	current := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("expired@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkQuotaExhausted("expired@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	current = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

	full := 1.0
	manager.UpdateAccountQuota("expired@example.com", Quota{
		Models:      map[string]ModelQuota{"gemini-3.8-flash-high": {RemainingFraction: &full}},
		LastChecked: current.UnixMilli(),
	}, nil)

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 1 {
		t.Fatalf("past its reset time the catalog reading must win: %+v", got)
	}
	if got.ExhaustedUntilMS != 0 {
		t.Fatalf("an expired marker must not be carried: %+v", got)
	}
}

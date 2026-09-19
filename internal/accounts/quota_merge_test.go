package accounts

import (
	"context"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

func quotaTestManager(t *testing.T, now time.Time, account *Account) *Manager {
	t.Helper()
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestMergeQuotaFraction_OverwritesCatalogReading(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("merge@example.com")
	manager := quotaTestManager(t, now, account)

	full := 1.0
	manager.UpdateAccountQuota("merge@example.com", Quota{
		Models:      map[string]ModelQuota{"gemini-3.8-flash-high": {RemainingFraction: &full}},
		LastChecked: now.UnixMilli(),
	}, nil)

	low := 0.35
	manager.MergeQuotaFraction("merge@example.com", "gemini-3.8-flash-high", &low, "2026-09-20T00:00:00Z")
	got, ok := account.Quota.Models["gemini-3.8-flash-high"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.35 {
		t.Fatalf("live reading must overwrite catalog fraction: %+v %v", got, ok)
	}
	if got.ResetTime != "2026-09-20T00:00:00Z" {
		t.Fatalf("reset time not merged: %+v", got)
	}
}

func TestMergeQuotaFraction_NilFractionWithResetIsExhaustion(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("exhaust@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MergeQuotaFraction("exhaust@example.com", "some-bucket", nil, "2026-09-20T00:00:00Z")
	got, ok := account.Quota.Models["some-bucket"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.0 {
		t.Fatalf("nil fraction + reset must record 0.0: %+v %v", got, ok)
	}
}

func TestMergeQuotaFraction_IgnoresEmpty(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("empty@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MergeQuotaFraction("empty@example.com", "", nil, "")
	manager.MergeQuotaFraction("", "some-key", nil, "")
	manager.MergeQuotaFraction("empty@example.com", "no-signal", nil, "")
	if len(account.Quota.Models) != 0 {
		t.Fatalf("empty merges must not write: %+v", account.Quota.Models)
	}
}

func TestMergeQuotaPool_KeepsPoolsOutOfModels(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("pools@example.com")
	manager := quotaTestManager(t, now, account)

	half := 0.5
	manager.MergeQuotaPool("pools@example.com", "gemini-5h", &half, "2026-09-20T01:00:00Z")
	manager.MergeQuotaPool("pools@example.com", "", &half, "2026-09-20T01:00:00Z")

	got, ok := account.Quota.Pools["gemini-5h"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.5 {
		t.Fatalf("pool reading must land in Quota.Pools: %+v ok=%v", got, ok)
	}
	if got.ResetTime != "2026-09-20T01:00:00Z" {
		t.Fatalf("pool reset time not stored: %+v", got)
	}
	if len(account.Quota.Models) != 0 {
		t.Fatalf("pool reading must not enter Quota.Models: %+v", account.Quota.Models)
	}
	if account.Quota.LastChecked != now.UnixMilli() {
		t.Fatalf("pool merge must refresh LastChecked: %+v", account.Quota.LastChecked)
	}
}

func TestUpdateAccountCredits_StoresAndIgnoresEmpty(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("credits@example.com")
	manager := quotaTestManager(t, now, account)

	manager.UpdateAccountCredits("credits@example.com", map[string]int64{"GOOGLE_ONE_AI": 12345})
	if account.Credits["GOOGLE_ONE_AI"] != 12345 {
		t.Fatalf("credits not stored: %+v", account.Credits)
	}

	manager.UpdateAccountCredits("credits@example.com", nil)
	manager.UpdateAccountCredits("credits@example.com", map[string]int64{})
	if account.Credits["GOOGLE_ONE_AI"] != 12345 {
		t.Fatalf("empty update must not wipe balances: %+v", account.Credits)
	}

	manager.UpdateAccountCredits("credits@example.com", map[string]int64{"GOOGLE_ONE_AI": 12000})
	if account.Credits["GOOGLE_ONE_AI"] != 12000 {
		t.Fatalf("latest balance must win: %+v", account.Credits)
	}
}

// quotaCapableClient serves a catalog stuck at 1.0 plus a live summary at
// 0.4 and a stream carrying remaining_credits.
type quotaCapableClient struct{}

func (quotaCapableClient) LoadCodeAssist(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":{"id":"p"}}`)}, nil
}

func (quotaCapableClient) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: 200, Body: []byte(`{
		"models": {"gemini-3.8-flash-high": {"displayName": "Gemini 3.8 Flash (High)",
			"quotaInfo": {"remainingFraction": 1.0, "resetTime": ""}}},
		"agentModelSorts": [{"groups": [{"modelIds": ["gemini-3.8-flash-high"]}]}]
	}`)}, nil
}

func (quotaCapableClient) RetrieveUserQuotaSummary(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: 200, Body: []byte(`{
		"buckets": [{"bucketId": "gemini-3.8-flash-high", "displayName": "Gemini 3.8 Flash (High)",
			"remainingFraction": 0.4, "resetTime": "2026-09-20T00:00:00Z"}]
	}`)}, nil
}

func (quotaCapableClient) RetrieveUserQuota(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: 200, Body: []byte(`{"buckets": []}`)}, nil
}

func (quotaCapableClient) StreamGenerateContent(_ context.Context, _ any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if consume != nil {
		_ = consume(cloudcode.SSEEvent{Data: []byte(`{"remainingCredits": [{"creditType": "GOOGLE_ONE_AI", "creditAmount": 777}]}`)})
	}
	return cloudcode.Response{StatusCode: 200}, nil
}

func TestFetchAvailableModelsMergesLiveQuota(t *testing.T) {
	dispatcher := newDispatcherWithClient(t, quotaCapableClient{})

	if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := dispatcher.manager.accounts[0]
	got, ok := account.Quota.Models["gemini-3.8-flash-high"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.4 {
		t.Fatalf("summary fraction must overwrite static catalog 1.0: %+v %v", got, ok)
	}
}

func TestStreamGenerateContentRecordsRemainingCredits(t *testing.T) {
	dispatcher := newDispatcherWithClient(t, quotaCapableClient{})

	_, err := dispatcher.StreamGenerateContent(context.Background(),
		map[string]any{"model": "gemini-3.8-flash-high"},
		func(cloudcode.SSEEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := dispatcher.manager.accounts[0].Credits["GOOGLE_ONE_AI"]; got != 777 {
		t.Fatalf("remaining credits must be recorded, got %+v", dispatcher.manager.accounts[0].Credits)
	}
}

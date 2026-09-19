package cloudcode

import (
	"testing"
)

func TestParseQuotaSummary_BucketsAndGroups(t *testing.T) {
	body := []byte(`{
		"buckets": [
			{"bucketId": "gemini-3.8-flash-high", "displayName": "Gemini 3.8 Flash (High)",
			 "remainingFraction": 0.62, "resetTime": "2026-09-20T00:00:00Z"},
			{"bucketId": "exhausted-bucket", "displayName": "Exhausted",
			 "resetTime": "2026-09-20T01:00:00Z"}
		],
		"groups": [
			{"displayName": "Flash", "buckets": [
				{"bucketId": "gemini-3.8-flash-low", "displayName": "Gemini Low",
				 "remainingFraction": 0.9, "resetTime": ""}
			]}
		]
	}`)
	buckets := ParseQuotaSummary(body)
	if len(buckets) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(buckets))
	}
	if buckets[0].ID != "gemini-3.8-flash-high" || buckets[0].RemainingFraction == nil || *buckets[0].RemainingFraction != 0.62 {
		t.Errorf("top-level bucket mismatch: %+v", buckets[0])
	}
	if buckets[1].RemainingFraction != nil {
		t.Errorf("exhausted bucket must have nil fraction here (manager maps it): %+v", buckets[1])
	}
	if buckets[2].ID != "gemini-3.8-flash-low" {
		t.Errorf("group bucket not flattened: %+v", buckets[2])
	}
}

func TestParseQuotaSummary_EmptyAndGarbage(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte("  "), []byte("not json"), []byte(`{"other": 1}`)} {
		if buckets := ParseQuotaSummary(body); len(buckets) != 0 {
			t.Errorf("expected no buckets for %q, got %+v", body, buckets)
		}
	}
}

func TestParseUserQuota_TokenTypes(t *testing.T) {
	body := []byte(`{
		"buckets": [
			{"modelId": "gemini-3.8-flash-high", "tokenType": "REQUESTS",
			 "remainingFraction": 0.4, "resetTime": "2026-09-20T00:00:00Z"},
			{"modelId": "gemini-3.8-flash-low", "tokenType": 2,
			 "remainingAmount": 0, "resetTime": "2026-09-20T00:00:00Z"},
			{"tokenType": "REQUESTS", "remainingFraction": 0.5}
		]
	}`)
	buckets := ParseUserQuota(body)
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets (model-less skipped), got %+v", buckets)
	}
	if buckets[0].TokenType != "REQUESTS" || *buckets[0].RemainingFraction != 0.4 {
		t.Errorf("string tokenType mismatch: %+v", buckets[0])
	}
	if buckets[1].TokenType != "WTUS" || buckets[1].RemainingAmount == nil || *buckets[1].RemainingAmount != 0 {
		t.Errorf("numeric tokenType / amount mismatch: %+v", buckets[1])
	}
}

func TestParseRemainingCredits(t *testing.T) {
	camel := []byte(`{"response": {}, "consumedCredits": [{"creditType": "GOOGLE_ONE_AI", "creditAmount": 5}],
		"remainingCredits": [{"creditType": "GOOGLE_ONE_AI", "creditAmount": 12345}]}`)
	balances, ok := ParseRemainingCredits(camel)
	if !ok || len(balances) != 1 || balances[0].CreditType != "GOOGLE_ONE_AI" || balances[0].Amount != 12345 {
		t.Fatalf("camelCase parse failed: %+v %v", balances, ok)
	}

	snake := []byte(`{"remaining_credits": [{"credit_type": 1, "credit_amount": 999}]}`)
	balances, ok = ParseRemainingCredits(snake)
	if !ok || len(balances) != 1 || balances[0].CreditType != "GOOGLE_ONE_AI" || balances[0].Amount != 999 {
		t.Fatalf("snake_case parse failed: %+v %v", balances, ok)
	}

	for _, body := range [][]byte{
		[]byte(`{"response": {"candidates": []}}`),
		[]byte(`not json at all {{{`),
		[]byte(`{"remainingCredits": []}`),
	} {
		if _, ok := ParseRemainingCredits(body); ok {
			t.Errorf("expected no credit signal for %q", body)
		}
	}
}

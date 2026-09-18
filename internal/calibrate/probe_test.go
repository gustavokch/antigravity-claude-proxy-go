package calibrate

import (
	"errors"
	"testing"
	"time"
)

// dailyBody is the verbatim daily-endpoint rejection recorded in
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md §6.1.
const dailyBody = `{ "error": { "code": 429, "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 17m31s.", "status": "RESOURCE_EXHAUSTED", "details": [ { "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED", "domain": "cloudcode-pa.googleapis.com", "metadata": { "uiMessage": "true", "model": "gemini-3.8-flash-high", "quotaResetDelay": "17m31.337247485s", "quotaResetTimeStamp": "2026-09-18T00:58:20Z" } }, { "@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1051.337247485s" } ] } }`

func TestParseRetryDelayReadsTheRecordedDailyBody(t *testing.T) {
	delay, ok := ParseRetryDelay(dailyBody)

	if !ok {
		t.Fatal("ParseRetryDelay: got not found on the recorded daily body")
	}
	if delay.Round(time.Second) != 1051*time.Second {
		t.Fatalf("delay: got %s, want 1051s", delay)
	}
}

func TestParseRetryDelayFallsBackToQuotaResetDelay(t *testing.T) {
	body := `{"error":{"details":[{"metadata":{"quotaResetDelay":"17m31.337247485s"}}]}}`

	delay, ok := ParseRetryDelay(body)

	if !ok {
		t.Fatal("ParseRetryDelay: got not found with only quotaResetDelay present")
	}
	if delay.Round(time.Second) != 17*time.Minute+31*time.Second {
		t.Fatalf("delay: got %s, want 17m31s", delay)
	}
}

func TestParseRetryDelayReportsNotFoundOnTheBareBody(t *testing.T) {
	body := `{ "error": { "code": 429, "message": "Resource has been exhausted (e.g. check quota).", "status": "RESOURCE_EXHAUSTED" } }`

	if _, ok := ParseRetryDelay(body); ok {
		t.Fatal("ParseRetryDelay: got found on the bare cloudcode-pa body, which carries no delay")
	}
}

func TestParseRetryDelayReportsNotFoundOnGarbage(t *testing.T) {
	if _, ok := ParseRetryDelay("not json at all"); ok {
		t.Fatal("ParseRetryDelay: got found on non-JSON input")
	}
}

func TestGuardBodyStopsOnValidationRequired(t *testing.T) {
	err := GuardBody(`{"error":{"status":"PERMISSION_DENIED","message":"VALIDATION_REQUIRED"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnADisabledAccount(t *testing.T) {
	err := GuardBody(`{"error":{"message":"This account has been disabled"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnATermsViolation(t *testing.T) {
	err := GuardBody(`{"error":{"message":"Gemini disabled for violation of terms of service"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnAPermanentAuthError(t *testing.T) {
	for _, body := range []string{`{"error":"invalid_grant"}`, `{"error":"unauthorized_client"}`} {
		if err := GuardBody(body); !errors.Is(err, ErrAccountProtected) {
			t.Fatalf("GuardBody(%s): got %v, want ErrAccountProtected", body, err)
		}
	}
}

func TestGuardBodyStopsOnAValidationURL(t *testing.T) {
	err := GuardBody(`{"error":{"message":"see validation_url https://accounts.google.com/signin/continue?x=1"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyPassesAnOrdinaryThrottle(t *testing.T) {
	if err := GuardBody(dailyBody); err != nil {
		t.Fatalf("GuardBody on an ordinary quota rejection: got %v, want nil", err)
	}
}

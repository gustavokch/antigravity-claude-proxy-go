package cloudcode

import (
	"bytes"
	"encoding/json"
)

// QuotaBucket is one live quota bucket from RetrieveUserQuotaSummary:
// either a top-level bucket or a bucket nested inside a group.
type QuotaBucket struct {
	ID                string
	DisplayName       string
	RemainingFraction *float64
	RemainingAmount   *int64
	ResetTime         string
	Disabled          bool
}

type quotaSummaryBucketJSON struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	Disabled          bool     `json:"disabled"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
	RemainingAmount   *int64   `json:"remainingAmount"`
}

type quotaSummaryGroupJSON struct {
	DisplayName string                   `json:"displayName"`
	Description string                   `json:"description"`
	Buckets     []quotaSummaryBucketJSON `json:"buckets"`
}

// ParseQuotaSummary flattens a RetrieveUserQuotaSummary JSON body into buckets.
// Unknown shapes (or empty bodies) yield no buckets and no error: the caller
// keeps the catalog fractions as fallback.
func ParseQuotaSummary(body []byte) []QuotaBucket {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var document struct {
		Buckets []quotaSummaryBucketJSON `json:"buckets"`
		Groups  []quotaSummaryGroupJSON  `json:"groups"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil
	}
	var out []QuotaBucket
	appendBucket := func(b quotaSummaryBucketJSON) {
		if b.BucketID == "" && b.DisplayName == "" {
			return
		}
		out = append(out, QuotaBucket{
			ID:                b.BucketID,
			DisplayName:       b.DisplayName,
			RemainingFraction: b.RemainingFraction,
			RemainingAmount:   b.RemainingAmount,
			ResetTime:         b.ResetTime,
			Disabled:          b.Disabled,
		})
	}
	for _, b := range document.Buckets {
		appendBucket(b)
	}
	for _, g := range document.Groups {
		for _, b := range g.Buckets {
			appendBucket(b)
		}
	}
	return out
}

// UserQuotaBucket is one entry from RetrieveUserQuota (model_id keyed,
// token-type dimensioned).
type UserQuotaBucket struct {
	ModelID           string
	TokenType         string
	RemainingFraction *float64
	RemainingAmount   *int64
	ResetTime         string
}

// ParseUserQuota decodes a RetrieveUserQuota JSON body. Oneof remains unknown
// shapes yield no buckets and no error.
func ParseUserQuota(body []byte) []UserQuotaBucket {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var document struct {
		Buckets []struct {
			ResetTime         string   `json:"resetTime"`
			TokenType         any      `json:"tokenType"`
			ModelID           string   `json:"modelId"`
			RemainingFraction *float64 `json:"remainingFraction"`
			RemainingAmount   *int64   `json:"remainingAmount"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil
	}
	var out []UserQuotaBucket
	for _, b := range document.Buckets {
		if b.ModelID == "" {
			continue
		}
		tokenType := ""
		switch v := b.TokenType.(type) {
		case string:
			tokenType = v
		case float64:
			// Proto enum on the wire as number: 1=REQUESTS, 2=WTUs.
			switch int(v) {
			case 1:
				tokenType = "REQUESTS"
			case 2:
				tokenType = "WTUS"
			}
		}
		out = append(out, UserQuotaBucket{
			ModelID:           b.ModelID,
			TokenType:         tokenType,
			RemainingFraction: b.RemainingFraction,
			RemainingAmount:   b.RemainingAmount,
			ResetTime:         b.ResetTime,
		})
	}
	return out
}

// CreditBalance is a remaining credit amount keyed by credit-type name
// (e.g. "GOOGLE_ONE_AI"), as reported by GenerateContent responses.
type CreditBalance struct {
	CreditType string
	Amount     int64
}

// ParseRemainingCredits extracts remaining_credits from a GenerateContent
// (unary or SSE data-frame) JSON payload. ok is false when the payload
// carries no credit signal. Both camelCase and snake_case keys are accepted.
func ParseRemainingCredits(data []byte) (balances []CreditBalance, ok bool) {
	if !bytes.Contains(data, []byte("emaining")) || !bytes.Contains(data, []byte("redit")) {
		return nil, false
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, false
	}
	raw, exists := document["remainingCredits"]
	if !exists {
		raw, exists = document["remaining_credits"]
	}
	if !exists {
		return nil, false
	}
	// Unmarshal tolerant of a single object instead of an array.
	var entries []creditEntryJSON
	if err := json.Unmarshal(raw, &entries); err != nil {
		var single creditEntryJSON
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, false
		}
		entries = append(entries, single)
	}
	for _, e := range entries {
		typeName := creditTypeName(e.CreditType, e.CreditTypeSnake)
		amount := int64(0)
		hasAmount := false
		if e.CreditAmount != nil {
			amount, hasAmount = *e.CreditAmount, true
		} else if e.CreditAmountSnake != nil {
			amount, hasAmount = *e.CreditAmountSnake, true
		}
		if !hasAmount {
			continue
		}
		balances = append(balances, CreditBalance{CreditType: typeName, Amount: amount})
	}
	if len(balances) == 0 {
		return nil, false
	}
	return balances, true
}

// creditEntryJSON is one remaining/consumed credit entry in either key style.
type creditEntryJSON struct {
	CreditType        any    `json:"creditType"`
	CreditTypeSnake   any    `json:"credit_type"`
	CreditAmount      *int64 `json:"creditAmount"`
	CreditAmountSnake *int64 `json:"credit_amount"`
}

func creditTypeName(camel, snake any) string {
	for _, v := range []any{camel, snake} {
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			// Credits.CreditType enum: 1=GOOGLE_ONE_AI.
			if int(t) == 1 {
				return "GOOGLE_ONE_AI"
			}
			return "CREDIT_TYPE_UNSPECIFIED"
		}
	}
	return "CREDIT_TYPE_UNSPECIFIED"
}

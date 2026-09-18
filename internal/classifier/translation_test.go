package classifier

import (
	"encoding/json"
	"strings"
	"testing"
)

const anthropicClassifierRequest = `{
	"model": "claude-sonnet-5",
	"max_tokens": 64,
	"temperature": 0.2,
	"system": [{"type":"text","text":"You are a security monitor."}],
	"messages": [{"role":"user","content":[{"type":"text","text":"Action to review"}]}]
}`

func TestTranslateAnthropicToOpenAI(t *testing.T) {
	out, err := TranslateAnthropicToOpenAI([]byte(anthropicClassifierRequest), "local-judge", 0)
	if err != nil {
		t.Fatalf("TranslateAnthropicToOpenAI: %v", err)
	}

	var got struct {
		Model     string   `json:"model"`
		MaxTokens int      `json:"max_tokens"`
		Stream    bool     `json:"stream"`
		Temp      *float64 `json:"temperature"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}

	if got.Model != "local-judge" {
		t.Errorf("expected model local-judge, got %q", got.Model)
	}
	if got.MaxTokens != 64 {
		t.Errorf("expected max_tokens 64, got %d", got.MaxTokens)
	}
	if got.Stream {
		t.Error("translated requests must be non-streaming")
	}
	if got.Temp == nil || *got.Temp != 0.2 {
		t.Errorf("expected temperature 0.2, got %v", got.Temp)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected a system message plus one user message, got %d", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "security monitor") {
		t.Errorf("unexpected system message %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "user" || !strings.Contains(got.Messages[1].Content, "Action to review") {
		t.Errorf("unexpected user message %+v", got.Messages[1])
	}
}

func TestTranslateAnthropicToOpenAIAppliesMaxTokensOverride(t *testing.T) {
	out, err := TranslateAnthropicToOpenAI([]byte(anthropicClassifierRequest), "local-judge", 16)
	if err != nil {
		t.Fatalf("TranslateAnthropicToOpenAI: %v", err)
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MaxTokens != 16 {
		t.Errorf("expected the override to win, got %d", got.MaxTokens)
	}
}

func TestTranslateOpenAIToAnthropic(t *testing.T) {
	openaiResponse := []byte(`{
		"id": "chatcmpl-123",
		"choices": [{"index":0,"message":{"role":"assistant","content":"<severity>0</severity>"},"finish_reason":"stop"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
	}`)

	out, err := TranslateOpenAIToAnthropic(openaiResponse, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("TranslateOpenAIToAnthropic: %v", err)
	}

	var got struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal translated response: %v", err)
	}

	if got.Type != "message" || got.Role != "assistant" {
		t.Errorf("unexpected envelope: type=%q role=%q", got.Type, got.Role)
	}
	if !strings.HasPrefix(got.ID, "msg_clf_") {
		t.Errorf("expected a msg_clf_ id, got %q", got.ID)
	}
	if got.Model != "claude-sonnet-5" {
		t.Errorf("expected the echoed model, got %q", got.Model)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("expected end_turn, got %q", got.StopReason)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "<severity>0</severity>" {
		t.Fatalf("unexpected content %+v", got.Content)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 10 {
		t.Errorf("usage not carried over: %+v", got.Usage)
	}
}

func TestTranslateOpenAIToAnthropicMapsLengthFinish(t *testing.T) {
	out, err := TranslateOpenAIToAnthropic([]byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`), "m")
	if err != nil {
		t.Fatalf("TranslateOpenAIToAnthropic: %v", err)
	}
	var got struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StopReason != "max_tokens" {
		t.Errorf("expected max_tokens, got %q", got.StopReason)
	}
}

func TestTranslateOpenAIToAnthropicRejectsEmptyChoices(t *testing.T) {
	if _, err := TranslateOpenAIToAnthropic([]byte(`{"choices":[]}`), "m"); err == nil {
		t.Fatal("expected an error when the backend returned no choices")
	}
	if _, err := TranslateOpenAIToAnthropic([]byte(`not json`), "m"); err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}

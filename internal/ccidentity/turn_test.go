package ccidentity

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The conversation below mirrors the 2.1.304 multi-prompt capture
// (.reference/claude-code-headers-20261020-multiprompt.jsonl): three prompts in
// one process, the third followed by a tool round trip. The real client sent
// cc_turn_index 1, 2, 3, 3 and a new cc_prompt_id for each prompt, repeated on
// the follow-up. Message contents here are invented; what the test fixes is
// which messages count as a prompt.
const (
	promptOne    = `{"role":"user","content":[{"type":"text","text":"<system-reminder>claude.md</system-reminder>"},{"type":"text","text":"list the files"}]}`
	replyOne     = `{"role":"assistant","content":[{"type":"text","text":"done"}]}`
	promptTwo    = `{"role":"user","content":"and now the tests"}`
	replyTwo     = `{"role":"assistant","content":[{"type":"text","text":"ok"}]}`
	promptThree  = `{"role":"user","content":[{"type":"text","text":"run them"}]}`
	toolUseThree = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}`
	toolResult   = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"<system-reminder>todo list is empty</system-reminder>"}]}`
)

func conversation(messages ...string) []byte {
	return []byte(`{"model":"m","messages":[` + strings.Join(messages, ",") + `]}`)
}

var turnIndexRE = regexp.MustCompile(`cc_turn_index=(\d+);`)

// wireOf reads the three prompt values a request carries: the turn index and
// the prompt id from system[0], and the prompt id from the header.
func wireOf(t *testing.T, body []byte) (index int, billingPrompt, headerPrompt string) {
	t.Helper()
	id := testIdentity()
	turn := TurnOf(body)

	billing := BillingHeader(id, turn)
	match := turnIndexRE.FindStringSubmatch(billing)
	if match == nil {
		t.Fatalf("billing header carries no cc_turn_index: %q", billing)
	}
	index, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("cc_turn_index %q is not a number", match[1])
	}
	promptMatch := promptIDRE.FindStringSubmatch(billing)
	if promptMatch == nil {
		t.Fatalf("billing header carries no cc_prompt_id: %q", billing)
	}

	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), id, turn)
	return index, promptMatch[1], hdrGet(h, "x-claude-code-prompt-id")
}

// TestTurnOfFollowsThePromptSequenceOfTheCapture is the behaviour the capture
// shows and the proxy used to flatten: the turn index counts prompts, and a
// tool follow-up stays on its prompt.
func TestTurnOfFollowsThePromptSequenceOfTheCapture(t *testing.T) {
	requests := []struct {
		name      string
		body      []byte
		wantIndex int
	}{
		{"first prompt", conversation(promptOne), 1},
		{"second prompt", conversation(promptOne, replyOne, promptTwo), 2},
		{"third prompt", conversation(promptOne, replyOne, promptTwo, replyTwo, promptThree), 3},
		{"third prompt's tool follow-up", conversation(promptOne, replyOne, promptTwo, replyTwo, promptThree, toolUseThree, toolResult), 3},
	}

	var ids []string
	for _, request := range requests {
		index, billing, header := wireOf(t, request.body)
		if index != request.wantIndex {
			t.Errorf("%s: cc_turn_index = %d, want %d", request.name, index, request.wantIndex)
		}
		if header != billing {
			t.Errorf("%s: x-claude-code-prompt-id = %s but cc_prompt_id = %s; the capture shows one value in both", request.name, header, billing)
		}
		ids = append(ids, billing)
	}

	// A new id per prompt, the same id across a prompt's follow-up.
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Errorf("three prompts shared a prompt id: %v", ids[:3])
	}
	if ids[2] != ids[3] {
		t.Errorf("the tool follow-up of the third prompt changed its id: %s then %s", ids[2], ids[3])
	}
}

// TestTurnOfIsTheSameForAReplay: a cache bump, a retry and a failover attempt
// resend the same body and must present the same identity, which is what makes
// storing nothing between requests correct.
func TestTurnOfIsTheSameForAReplay(t *testing.T) {
	body := conversation(promptOne, replyOne, promptTwo)
	firstIndex, firstID, _ := wireOf(t, body)
	secondIndex, secondID, _ := wireOf(t, body)
	if firstIndex != secondIndex || firstID != secondID {
		t.Errorf("replay changed the identity: %d/%s then %d/%s", firstIndex, firstID, secondIndex, secondID)
	}
}

// TestTurnOfGivesACompactedConversationANewPromptID: a client that compacts its
// history sends fewer messages, so the prompt number goes back down. The real
// client never reuses a prompt id, and the number alone would.
func TestTurnOfGivesACompactedConversationANewPromptID(t *testing.T) {
	before := conversation(promptOne)
	compacted := conversation(`{"role":"user","content":"summary of the conversation so far"}`)

	beforeIndex, beforeID, _ := wireOf(t, before)
	compactedIndex, compactedID, _ := wireOf(t, compacted)
	if beforeIndex != 1 || compactedIndex != 1 {
		t.Fatalf("both conversations hold one prompt, got indexes %d and %d", beforeIndex, compactedIndex)
	}
	if beforeID == compactedID {
		t.Errorf("a different prompt at the same position reused prompt id %s", beforeID)
	}
}

func TestTurnOfSeparatesSessions(t *testing.T) {
	body := conversation(promptOne)
	turn := TurnOf(body)
	first := promptUUID(Identity{SessionKey: "session-a"}, turn)
	second := promptUUID(Identity{SessionKey: "session-b"}, turn)
	if first == second {
		t.Errorf("two sessions sent the same prompt id %s", first)
	}
}

// TestTurnOfCountsOnlyWhatAPersonSent pins the rule that decides which user
// messages advance the turn.
func TestTurnOfCountsOnlyWhatAPersonSent(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"unparseable body is the first prompt", []byte(`{"messages":`), 1},
		{"no messages is the first prompt", []byte(`{"model":"m"}`), 1},
		{"string content is a prompt", conversation(`{"role":"user","content":"hi"}`), 1},
		{"empty string is not", conversation(`{"role":"user","content":"  "}`, promptTwo), 1},
		{"a reminder alone is not", conversation(`{"role":"user","content":"<system-reminder>x</system-reminder>"}`, promptTwo), 1},
		{"tool output alone is not", conversation(promptOne, toolUseThree, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}`), 1},
		{"tool output beside typed text is", conversation(promptOne, toolUseThree, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"also, stop"}]}`), 2},
		{"an attachment is", conversation(`{"role":"user","content":[{"type":"image","source":{}}]}`, replyOne, promptTwo), 2},
		{"assistant messages never count", conversation(replyOne, replyTwo, promptTwo), 1},
		{"reminder and tool output are not prompts", conversation(promptOne, replyOne, toolResult), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnOf(tc.body).PromptNumber; got != tc.want {
				t.Errorf("PromptNumber = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestZeroTurnIsTheFirstPrompt keeps the documented zero value honest for any
// caller that does not read a body.
func TestZeroTurnIsTheFirstPrompt(t *testing.T) {
	if got := BillingHeader(testIdentity(), Turn{}); !strings.Contains(got, "cc_turn_index=1;") {
		t.Errorf("zero Turn billing header = %q, want cc_turn_index=1", got)
	}
}

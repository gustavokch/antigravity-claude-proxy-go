package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// variantEnv wires a Server whose Zen gateway points at a counting fake. It
// returns the server, how many times Zen was called, the account-backed
// recorder and the capture directory.
func variantEnv(t *testing.T, zenStatus int, zenBody string, mutate func(*config.Config)) (*Server, *atomic.Int32, *dispatchRecordingBackend, string) {
	t.Helper()
	withHarnessDefaults(t)
	t.Setenv("OPENCODE_API_KEY", "")
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/zen/v1/systemone" {
			t.Errorf("Zen was called at %q, want /zen/v1/systemone", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(zenStatus)
		_, _ = w.Write([]byte(zenBody))
	}))
	t.Cleanup(upstream.Close)

	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	server, accountBackend := newAccountBackedTestServer(t)
	server.classifierAudit = classifier.NewRecorder(10)
	dir := t.TempDir()

	cfg := config.Get()
	cfg.Zen.Enabled = true
	cfg.Zen.APIKey = "sk-zen-test"
	cfg.Zen.BaseURL = upstream.URL + "/zen"
	cfg.Classifier.Enabled = true
	cfg.Classifier.Action = config.ActionAlwaysStub
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	if mutate != nil {
		mutate(&cfg)
	}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)
	return server, &hits, accountBackend, dir
}

// With no rules and no variant backend configured, a Zen-enabled deployment
// answers Stage 1 and block-prefilter from Jev. Everything Jev declines goes
// on to the teacher path, here the always_stub canned verdict.
func TestBuiltinJevAnswersTheClassifierVariantsByDefault(t *testing.T) {
	const stubStage1 = "<severity>0</severity>"
	cases := []struct {
		name        string
		footer      string
		zenStatus   int
		zenBody     string
		wantHits    int32
		wantVerdict string
		wantAudit   classifier.EventStatus
		wantSource  corpus.Source
	}{
		{name: "stage 1 allow", footer: classifierStage1Footer, zenStatus: 200, zenBody: jevBenignAnswer, wantHits: 1, wantVerdict: "<severity>0</severity>", wantAudit: classifier.EventStatusRerouted, wantSource: corpus.SourceJev},
		{name: "stage 1 band D escalates", footer: classifierStage1Footer, zenStatus: 200, zenBody: jevRiskyAnswer, wantHits: 1, wantVerdict: stubStage1, wantAudit: classifier.EventStatusEscalated, wantSource: corpus.SourceStub},
		{name: "stage 1 zen failure falls through", footer: classifierStage1Footer, zenStatus: 429, zenBody: `{"error":"rate limited"}`, wantHits: 1, wantVerdict: stubStage1, wantAudit: classifier.EventStatusError, wantSource: corpus.SourceStub},
		{name: "block-prefilter allow", footer: classifierBlockFooter, zenStatus: 200, zenBody: jevBlockNoAnswer, wantHits: 1, wantVerdict: "<block>no</block>", wantAudit: classifier.EventStatusRerouted, wantSource: corpus.SourceJev},
		{name: "block-prefilter uncertain escalates", footer: classifierBlockFooter, zenStatus: 200, zenBody: jevBlockMaybeAnswer, wantHits: 1, wantAudit: classifier.EventStatusEscalated, wantSource: corpus.SourceStub},
		{name: "block-prefilter yes escalates", footer: classifierBlockFooter, zenStatus: 200, zenBody: jevBlockYesAnswer, wantHits: 1, wantAudit: classifier.EventStatusEscalated, wantSource: corpus.SourceStub},
		{name: "stage 2 is never sent", footer: layaStage2Footer, zenStatus: 200, zenBody: jevBenignAnswer, wantHits: 0, wantVerdict: "<thinking>" + config.DefaultConfig().Classifier.DefaultThinking + "</thinking><severity>0</severity>", wantSource: corpus.SourceStub},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, hits, accountBackend, dir := variantEnv(t, testCase.zenStatus, testCase.zenBody, nil)

			rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, testCase.footer))

			if hits.Load() != testCase.wantHits {
				t.Errorf("Zen was called %d times, want %d", hits.Load(), testCase.wantHits)
			}
			if accountBackend.hit {
				t.Error("an account-backed upstream was called; a Jev answer must not consume account capacity")
			}
			if testCase.wantVerdict != "" {
				if got := verdictTextFrom(t, rec.Body.Bytes()); got != testCase.wantVerdict {
					t.Errorf("client received %q, want %q", got, testCase.wantVerdict)
				}
			} else if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<block>no") {
				t.Errorf("an escalated block-prefilter request was answered %q; only the teacher may decline to say no", rec.Body.String())
			}
			history := server.classifierAudit.History()
			if testCase.wantAudit == "" {
				if len(history) != 0 {
					t.Errorf("audit = %+v, want no event for a request never sent to Jev", history)
				}
			} else if len(history) != 1 || history[0].Status != testCase.wantAudit {
				t.Fatalf("audit = %+v, want one %s event", history, testCase.wantAudit)
			}
			rows := readCaptureRows(t, server, dir)
			if len(rows) != 1 || rows[0].Source != testCase.wantSource {
				t.Fatalf("capture rows = %+v, want one %s row", rows, testCase.wantSource)
			}
		})
	}
}

// The built-in route is opt-out per variant, and never applies without the
// operator having enabled Zen and supplied a key: it sends the graded action
// off the machine.
func TestBuiltinJevRouteIsSwitchedOffWhereTheOperatorDidNotOptIn(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{name: "zen disabled", mutate: func(cfg *config.Config) { cfg.Zen.Enabled = false }},
		{name: "no zen key", mutate: func(cfg *config.Config) { cfg.Zen.APIKey = "" }},
		{name: "variant set to off", mutate: func(cfg *config.Config) {
			cfg.Classifier.Variants = map[string]config.ClassifierVariantConfig{
				"stage1-severity": {TargetBackend: config.VariantBackendOff},
			}
		}},
		{name: "passthrough collection window", mutate: func(cfg *config.Config) { cfg.Classifier.Action = config.ActionPassthrough }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, hits, _, _ := variantEnv(t, 200, jevBenignAnswer, testCase.mutate)

			postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
			// The capture writer is asynchronous; wait so TempDir cleanup
			// does not race a row still being written.
			server.classifierCorpus.Load().Wait()

			if hits.Load() != 0 {
				t.Errorf("Zen was called %d times, want 0", hits.Load())
			}
		})
	}
}

// The built-in route escalates band C, because severity 15 sits under Claude
// Code's Stage 1 threshold of 25 (claude-sonnet-5) and would read as an allow
// of an action the teacher scores 25-49. A bare jev backend keeps its own
// default of D only.
func TestBuiltinJevEscalatesBandCButABareJevBackendDoesNot(t *testing.T) {
	const bandC = `{"model":"jev-1.13-free","answers":{"risk":{"type":"choice","choice":"C","confidence":0.9,"probabilities":{"A":0,"B":0.1,"C":0.9,"D":0}}},"usage":{"input_tokens":400,"output_tokens":45}}`

	server, hits, _, _ := variantEnv(t, 200, bandC, nil)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
	server.classifierCorpus.Load().Wait()
	if hits.Load() != 1 {
		t.Fatalf("Zen was called %d times, want 1", hits.Load())
	}
	history := server.classifierAudit.History()
	if len(history) != 1 || history[0].Status != classifier.EventStatusEscalated {
		t.Errorf("built-in route: audit = %+v, want one escalated event for band C (response %s)", history, rec.Body.String())
	}

	upstream, _ := jevUpstream(t, bandC)
	withZenKeys(t, "sk-zen-test", "")
	withHarnessDefaults(t)
	message, err := (&Server{}).callClassifierBackend(t.Context(), jevCall(jevBackend(upstream.URL+"/v1/systemone")))
	if err != nil {
		t.Fatalf("bare jev backend: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>15</severity>" {
		t.Errorf("bare jev backend verdict = %q, want band C mapped to 15", got)
	}
}

// A variant can name any configured backend, and an unknown name falls back to
// the legacy path rather than black-holing the request.
func TestVariantTargetBackendNamesAConfiguredBackend(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jevBenignAnswer))
	}))
	t.Cleanup(other.Close)

	server, zenHits, _, _ := variantEnv(t, 200, jevBenignAnswer, func(cfg *config.Config) {
		cfg.Classifier.Variants = map[string]config.ClassifierVariantConfig{
			"stage1-severity": {TargetBackend: "mine"},
			"block-prefilter": {TargetBackend: "ghost"},
		}
		cfg.Classifier.Backends = map[string]config.TargetBackend{
			"mine": {Name: "mine", URL: other.URL + "/v1/systemone", Format: config.BackendFormatJev},
		}
	})

	postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
	postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierBlockFooter))
	server.classifierCorpus.Load().Wait()

	if hits.Load() != 1 {
		t.Errorf("named backend was called %d times, want 1 (stage 1 only)", hits.Load())
	}
	if zenHits.Load() != 0 {
		t.Errorf("the built-in Zen route was called %d times, want 0: a named variant backend replaces it", zenHits.Load())
	}
}

// A request an operator rule already sent to a backend must not be sent to
// Zen a second time by the built-in route when that backend escalates or fails.
func TestBuiltinJevIsSkippedWhenARuleAlreadyHandledTheRequest(t *testing.T) {
	server, zenHits, _, _ := variantEnv(t, 200, jevBenignAnswer, nil)
	var ruleHits atomic.Int32
	ruleBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ruleHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jevRiskyAnswer))
	}))
	t.Cleanup(ruleBackend.Close)

	cfg := config.Get()
	cfg.Classifier.Rules = []config.Rule{{
		ID: "jev", Name: "Jev", Enabled: true, Action: config.RuleActionReroute, TargetBackend: "zen-jev",
		Conditions: config.RuleConditions{SystemPromptPatterns: []config.MatchPattern{{Type: config.PatternSubstring, Pattern: "You are a security monitor"}}},
	}}
	cfg.Classifier.Backends = map[string]config.TargetBackend{
		"zen-jev": {Name: "zen-jev", URL: ruleBackend.URL + "/v1/systemone", Format: config.BackendFormatJev},
	}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
	server.classifierCorpus.Load().Wait()

	if ruleHits.Load() != 1 {
		t.Errorf("the rule's backend was called %d times, want 1", ruleHits.Load())
	}
	if zenHits.Load() != 0 {
		t.Errorf("the built-in route called Zen %d times after a rule already handled the request, want 0", zenHits.Load())
	}
}

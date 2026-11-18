package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mixedCanonicalRequest() Request {
	return Request{
		Input: "Customer charged twice and Safari export is broken.",
		Questions: []Question{
			{
				Type:         QuestionPredicate,
				Name:         "is_breaking",
				Instructions: "Does this report a breaking issue?",
			},
			{
				Type:         QuestionChoice,
				Name:         "department",
				Instructions: "Which department owns this ticket?",
				Choices: []ChoiceOption{
					{Value: StringChoice("billing"), Description: "Payments and refunds."},
					{Value: StringChoice("technical"), Description: "Product defects."},
					{Value: StringChoice("other"), Description: "Everything else."},
				},
			},
			{
				Type:         QuestionChoice,
				Name:         "needs_human",
				Instructions: "Should a human escalate this ticket?",
				Choices: []ChoiceOption{
					{Value: BoolChoice(true), Description: "Escalate immediately."},
					{Value: BoolChoice(false), Description: "Automated handling."},
				},
			},
			{
				Type:         QuestionScore,
				Name:         "severity",
				Instructions: "Rate the severity of the issue.",
				Levels: []ScoreLevel{
					{Label: "Cosmetic", Description: "Appearance only."},
					{Label: "Workaround available", Description: "Another path works."},
					{Label: "Fully blocked", Description: "No workaround."},
				},
			},
		},
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMixedQuestionsAcrossAllProviders(t *testing.T) {
	req := mixedCanonicalRequest()

	t.Run("openai", func(t *testing.T) {
		fixture := readFixture(t, "openai_mixed_response.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer openai-test-key" {
				t.Errorf("unexpected request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			var wire map[string]any
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if wire["model"] != OpenAIDefaultModel || wire["input"] != req.Input {
				t.Errorf("unexpected wire top-level: %#v", wire)
			}
			questions := wire["questions"].([]any)
			if len(questions) != 4 {
				t.Fatalf("expected 4 questions, got %d", len(questions))
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		var eval Evaluator = Client{
			Provider: ProviderOpenAI,
			APIKey:   "openai-test-key",
			Endpoint: srv.URL + "/v1/decisions",
		}
		answers, err := eval.Decide(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderOpenAI, "gpt-6-luna", false, 1.1)
	})

	t.Run("typesafe", func(t *testing.T) {
		fixture := readFixture(t, "typesafe_mixed_response.json")
		tsReq := mixedCanonicalRequest()
		tsReq.Questions[0].Criteria = &PredicateCriteria{
			TrueDescription:  "Confirmed regression",
			FalseDescription: "Cosmetic or question only",
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer typesafe-test-key" {
				t.Errorf("unexpected request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			var wire struct {
				Model     string                     `json:"model"`
				State     string                     `json:"state"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if wire.Model != TypeSafeDefaultModel || wire.State != tsReq.Input || len(wire.Questions) != 4 {
				t.Fatalf("unexpected TypeSafe wire request: %+v", wire)
			}
			var noulQ struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			}
			if err := json.Unmarshal(wire.Questions["is_breaking"], &noulQ); err != nil {
				t.Fatal(err)
			}
			if noulQ.Type != "noul" || noulQ.Criteria["true"] != "Confirmed regression" || noulQ.Criteria["false"] != "Cosmetic or question only" {
				t.Fatalf("unexpected TypeSafe noul serialization: %+v", noulQ)
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		answers, err := (Client{
			Provider: ProviderTypeSafe,
			APIKey:   "typesafe-test-key",
			Endpoint: srv.URL + "/v1/systemone",
		}).Decide(context.Background(), tsReq)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderTypeSafe, "jev-1.14.0", false, 1.1)
	})

	t.Run("gemini", func(t *testing.T) {
		fixture := readFixture(t, "gemini_mixed_response.json")
		gemReq := mixedCanonicalRequest()
		gemReq.Questions[0].Criteria = &PredicateCriteria{
			TrueDescription:  "Confirmed regression",
			FalseDescription: "Cosmetic or question only",
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1beta/decisions" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			if r.Header.Get("x-goog-api-key") != "gemini-test-key" || r.Header.Get("Authorization") != "" {
				t.Errorf("unexpected auth headers: x-goog-api-key=%q auth=%q", r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"))
			}
			var wire struct {
				Model     string                     `json:"model"`
				Input     string                     `json:"input"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if wire.Model != GeminiDefaultModel || wire.Input != gemReq.Input || len(wire.Questions) != 4 {
				t.Fatalf("unexpected Gemini wire request: %+v", wire)
			}
			var noulQ struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			}
			if err := json.Unmarshal(wire.Questions["is_breaking"], &noulQ); err != nil {
				t.Fatal(err)
			}
			if noulQ.Type != "noul" || noulQ.Criteria["true_description"] != "Confirmed regression" || noulQ.Criteria["false_description"] != "Cosmetic or question only" {
				t.Fatalf("unexpected Gemini noul serialization: %+v", noulQ)
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		answers, err := (Client{
			Provider: ProviderGemini,
			APIKey:   "gemini-test-key",
			Endpoint: srv.URL + "/v1beta/decisions",
		}).Decide(context.Background(), gemReq)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderGemini, "", true, 1.12)
	})

	t.Run("openrouter", func(t *testing.T) {
		fixture := readFixture(t, "openrouter_mixed_response.json")
		orReq := mixedCanonicalRequest()
		orReq.Questions[0].Criteria = &PredicateCriteria{
			TrueDescription:  "Confirmed regression",
			FalseDescription: "Cosmetic or question only",
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer openrouter-test-key" {
				t.Errorf("unexpected OpenRouter request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			var wire struct {
				Model     string                     `json:"model"`
				State     string                     `json:"state"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if wire.Model != OpenRouterDefaultModel || wire.State != orReq.Input || len(wire.Questions) != 4 {
				t.Fatalf("unexpected OpenRouter wire request: %+v", wire)
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		// Bare server URL normalizes to /api/alpha/decisions.
		answers, err := (Client{
			Provider: ProviderOpenRouter,
			APIKey:   "openrouter-test-key",
			Endpoint: srv.URL,
		}).Decide(context.Background(), orReq)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderOpenRouter, "typesafe/jev-1.14-20261014", false, 1.1)
	})

	t.Run("litellm_openai_route", func(t *testing.T) {
		fixture := readFixture(t, "litellm_mixed_response.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer litellm-test-key" {
				t.Errorf("unexpected LiteLLM request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			var wire map[string]any
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if wire["model"] != "jev" {
				t.Errorf("expected gateway alias model 'jev', got %v", wire["model"])
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		// Base URL without path must normalize to /v1/decisions.
		answers, err := (Client{
			Provider: ProviderLiteLLM,
			Model:    "jev",
			APIKey:   "litellm-test-key",
			Endpoint: srv.URL,
		}).Decide(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderLiteLLM, "typesafe/jev-1.14.0", false, 1.1)
	})

	t.Run("litellm_systemone_route", func(t *testing.T) {
		fixture := readFixture(t, "typesafe_mixed_response.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer litellm-test-key" {
				t.Errorf("unexpected LiteLLM systemone request: %s", r.URL.Path)
			}
			w.Write(fixture)
		}))
		defer srv.Close()

		answers, err := (Client{
			Provider: ProviderLiteLLM,
			Model:    "jev",
			APIKey:   "litellm-test-key",
			Endpoint: srv.URL + "/v1/systemone",
		}).Decide(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		assertMixedAnswers(t, answers, ProviderLiteLLM, "jev-1.14.0", false, 1.1)
	})
}

func assertMixedAnswers(
	t *testing.T,
	answers map[string]Answer,
	wantProvider Provider,
	wantModel string,
	wantPredicateConfidence bool,
	wantScore float64,
) {
	t.Helper()
	pred := answers["is_breaking"]
	if pred.Type != AnswerPredicate || pred.Probability == nil || *pred.Probability != 0.92 || pred.Provider != wantProvider || pred.Model != wantModel {
		t.Fatalf("unexpected predicate answer: %+v", pred)
	}
	if wantPredicateConfidence && (pred.Confidence == nil || *pred.Confidence != 0.92) {
		t.Fatalf("expected predicate confidence on %s, got %+v", wantProvider, pred)
	}
	if !wantPredicateConfidence && pred.Confidence != nil {
		t.Fatalf("did not expect fabricated predicate confidence on %s, got %v", wantProvider, *pred.Confidence)
	}

	strChoice := answers["department"]
	if strChoice.Type != AnswerChoice || strChoice.Choice == nil || !strChoice.Choice.IsString() {
		t.Fatalf("expected string choice, got %+v", strChoice)
	}
	if s, _ := strChoice.Choice.StringValue(); s != "billing" {
		t.Fatalf("expected billing, got %q", s)
	}
	if len(strChoice.ChoiceProbabilities) != 3 || strChoice.ChoiceProbabilities[0].Value != StringChoice("billing") {
		t.Fatalf("choice probabilities order/values wrong: %+v", strChoice.ChoiceProbabilities)
	}

	boolChoice := answers["needs_human"]
	if boolChoice.Type != AnswerChoice || boolChoice.Choice == nil || !boolChoice.Choice.IsBool() {
		t.Fatalf("expected boolean choice preserved, got %+v", boolChoice)
	}
	if b, ok := boolChoice.Choice.Bool(); !ok || !b {
		t.Fatalf("expected boolean true choice, got %+v", boolChoice.Choice)
	}
	if len(boolChoice.ChoiceProbabilities) != 2 || boolChoice.ChoiceProbabilities[0].Value != BoolChoice(true) || boolChoice.ChoiceProbabilities[1].Value != BoolChoice(false) {
		t.Fatalf("boolean choice probabilities lost type: %+v", boolChoice.ChoiceProbabilities)
	}

	score := answers["severity"]
	if score.Type != AnswerScore || score.Score == nil || math.Abs(*score.Score-wantScore) > 1e-9 {
		t.Fatalf("unexpected score answer: %+v", score)
	}
	if len(score.ScoreProbabilities) != 3 || score.ScoreProbabilities[1].Label != "Workaround available" {
		t.Fatalf("unexpected score probabilities: %+v", score.ScoreProbabilities)
	}
}

func TestSameCanonicalPredicateRequestAcrossNativeProviders(t *testing.T) {
	req, err := BuildRequest("diff: - old\n+ new", "", Checks{"changes_behavior": "Does this change behavior?"})
	if err != nil {
		t.Fatal(err)
	}

	openAISrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"changes_behavior","probability":0.85}]}`)
	}))
	defer openAISrv.Close()

	typeSafeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"jev-1.14.0","answers":{"changes_behavior":{"type":"noul","noul":0.85}}}`)
	}))
	defer typeSafeSrv.Close()

	geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"answers":{"changes_behavior":{"type":"noul","noul":0.85,"confidence":0.85}}}`)
	}))
	defer geminiSrv.Close()

	clients := []Client{
		{Provider: ProviderOpenAI, APIKey: "k1", Endpoint: openAISrv.URL},
		{Provider: ProviderTypeSafe, APIKey: "k2", Endpoint: typeSafeSrv.URL},
		{Provider: ProviderGemini, APIKey: "k3", Endpoint: geminiSrv.URL},
		{Provider: ProviderOpenRouter, APIKey: "k4", Endpoint: typeSafeSrv.URL + "/api/alpha/decisions"},
	}
	for _, client := range clients {
		answers, err := client.Decide(context.Background(), req)
		if err != nil {
			t.Fatalf("%s failed: %v", client.Provider, err)
		}
		ans := answers["changes_behavior"]
		if ans.Type != AnswerPredicate || ans.Name != "changes_behavior" || ans.Probability == nil || *ans.Probability != 0.85 {
			t.Fatalf("%s normalized answer mismatch: %+v", client.Provider, ans)
		}
	}
}

func TestOpenAIStringAndBoolDistinctInSameChoiceQuestion(t *testing.T) {
	req := Request{
		Input: "value is literal string true",
		Questions: []Question{{
			Type:         QuestionChoice,
			Name:         "kind",
			Instructions: "Pick the exact typed token.",
			Choices: []ChoiceOption{
				{Value: BoolChoice(true), Description: "boolean true"},
				{Value: StringChoice("true"), Description: "string true"},
			},
		}},
	}

	// OpenAI natively distinguishes true vs "true".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"answers":[{"type":"choice","name":"kind","choice":"true","confidence":0.9,"probabilities":[{"value":true,"probability":0.1},{"value":"true","probability":0.9}]}]}`)
	}))
	defer srv.Close()

	answers, err := (Client{Provider: ProviderOpenAI, APIKey: "k", Endpoint: srv.URL}).Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !answers["kind"].Choice.IsString() {
		t.Fatalf("expected string 'true', got %+v", answers["kind"].Choice)
	}

	// TypeSafe, Gemini, OpenRouter, and LiteLLM must reject true + "true" collision with UnsupportedError.
	for _, p := range []Provider{ProviderTypeSafe, ProviderGemini, ProviderOpenRouter, ProviderLiteLLM} {
		_, err := (Client{Provider: p, Model: "m", Endpoint: "https://example.com/v1/decisions"}).Preview(req)
		if !IsUnsupported(err) {
			t.Fatalf("provider %s accepted colliding bool/string choices: %v", p, err)
		}
	}
}

func TestUnsupportedCapabilitiesProduceExplicitErrors(t *testing.T) {
	// 1. Box question type rejected.
	boxReq := Request{
		Input:     "image check",
		Questions: []Question{{Type: QuestionType("box"), Name: "target", Instructions: "find box"}},
	}
	if err := ValidateRequest(boxReq); !IsUnsupported(err) {
		t.Fatalf("expected UnsupportedError for box question, got %v", err)
	}

	// 2. Predicate criteria rejected on OpenAI and LiteLLM /v1/decisions, accepted on TypeSafe and Gemini.
	critReq := Request{
		Input: "refund check",
		Questions: []Question{{
			Type:         QuestionPredicate,
			Name:         "refund",
			Instructions: "Is this a refund?",
			Criteria:     &PredicateCriteria{TrueDescription: "Yes", FalseDescription: "No"},
		}},
	}
	for _, p := range []Provider{ProviderOpenAI, ProviderLiteLLM} {
		_, err := (Client{Provider: p, Model: "m", Endpoint: "https://example.com/v1/decisions"}).Preview(critReq)
		if !IsUnsupported(err) {
			t.Fatalf("expected UnsupportedError for predicate criteria on %s, got %v", p, err)
		}
	}
	for _, p := range []Provider{ProviderTypeSafe, ProviderGemini, ProviderOpenRouter} {
		if _, err := (Client{Provider: p}).Preview(critReq); err != nil {
			t.Fatalf("expected predicate criteria supported on %s, got %v", p, err)
		}
	}

	// 3. Score >10 levels supported on Gemini and OpenRouter (up to 255), rejected on OpenAI, TypeSafe, LiteLLM.
	levels := make([]ScoreLevel, 12)
	for i := range levels {
		levels[i] = ScoreLevel{Label: fmt.Sprintf("L%d", i)}
	}
	scoreReq := Request{
		Input:     "scale check",
		Questions: []Question{{Type: QuestionScore, Name: "scale", Instructions: "Rate.", Levels: levels}},
	}
	for _, p := range []Provider{ProviderGemini, ProviderOpenRouter} {
		if _, err := (Client{Provider: p}).Preview(scoreReq); err != nil {
			t.Fatalf("%s should allow 12 score levels: %v", p, err)
		}
	}
	for _, p := range []Provider{ProviderOpenAI, ProviderTypeSafe, ProviderLiteLLM} {
		_, err := (Client{Provider: p, Model: "m", Endpoint: "https://example.com/v1/decisions"}).Preview(scoreReq)
		if !IsUnsupported(err) {
			t.Fatalf("expected UnsupportedError for 12 score levels on %s, got %v", p, err)
		}
	}

	// 4. Gemini question name syntax check.
	badNameReq := Request{
		Input:     "evidence",
		Questions: []Question{{Type: QuestionPredicate, Name: "bad.name", Instructions: "Check?"}},
	}
	if _, err := (Client{Provider: ProviderGemini}).Preview(badNameReq); !IsUnsupported(err) {
		t.Fatalf("expected Gemini key syntax UnsupportedError, got %v", err)
	}
}

func TestInvalidChoiceAndScoreResponsesRejected(t *testing.T) {
	choiceReq := Request{
		Input: "evidence",
		Questions: []Question{{
			Type:         QuestionChoice,
			Name:         "pick",
			Instructions: "Pick one.",
			Choices: []ChoiceOption{
				{Value: BoolChoice(true)},
				{Value: BoolChoice(false)},
			},
		}},
	}
	openAIChoiceBad := map[string]string{
		"string instead of bool": `{"answers":[{"type":"choice","name":"pick","choice":"true","confidence":0.9,"probabilities":[{"value":true,"probability":0.9},{"value":false,"probability":0.1}]}]}`,
		"undeclared option":      `{"answers":[{"type":"choice","name":"pick","choice":"other","confidence":0.9,"probabilities":[{"value":true,"probability":0.9},{"value":false,"probability":0.1}]}]}`,
		"bad prob sum":           `{"answers":[{"type":"choice","name":"pick","choice":true,"confidence":0.9,"probabilities":[{"value":true,"probability":0.2},{"value":false,"probability":0.2}]}]}`,
		"missing option prob":    `{"answers":[{"type":"choice","name":"pick","choice":true,"confidence":0.9,"probabilities":[{"value":true,"probability":1.0}]}]}`,
		"duplicate option prob":  `{"answers":[{"type":"choice","name":"pick","choice":true,"confidence":0.9,"probabilities":[{"value":true,"probability":0.5},{"value":true,"probability":0.5}]}]}`,
		"null choice":            `{"answers":[{"type":"choice","name":"pick","choice":null,"confidence":0.9,"probabilities":[{"value":true,"probability":0.5},{"value":false,"probability":0.5}]}]}`,
		"missing confidence":     `{"answers":[{"type":"choice","name":"pick","choice":true,"probabilities":[{"value":true,"probability":0.5},{"value":false,"probability":0.5}]}]}`,
	}
	for name, body := range openAIChoiceBad {
		t.Run("openai_choice/"+name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			if _, err := (Client{Provider: ProviderOpenAI, APIKey: "k", Endpoint: srv.URL}).Decide(context.Background(), choiceReq); err == nil {
				t.Fatalf("accepted invalid choice response (%s)", name)
			}
		})
	}

	scoreReq := Request{
		Input: "evidence",
		Questions: []Question{{
			Type:         QuestionScore,
			Name:         "sev",
			Instructions: "Rate.",
			Levels:       []ScoreLevel{{Label: "Low"}, {Label: "High"}},
		}},
	}
	mapScoreBad := map[string]string{
		"duplicate answer key": `{"answers":{"sev":{"type":"score","score":0.5,"confidence":0.8,"probabilities":{"0":0.5,"1":0.5}},"sev":{"type":"score","score":0.5,"confidence":0.8,"probabilities":{"0":0.5,"1":0.5}}}}`,
		"duplicate prob key":   `{"answers":{"sev":{"type":"score","score":0.5,"confidence":0.8,"probabilities":{"0":0.5,"0":0.5,"1":0.5}}}}`,
		"score above max":      `{"answers":{"sev":{"type":"score","score":1.5,"confidence":0.8,"probabilities":{"0":0.5,"1":0.5}}}}`,
		"score negative":       `{"answers":{"sev":{"type":"score","score":-0.2,"confidence":0.8,"probabilities":{"0":0.5,"1":0.5}}}}`,
		"score null":           `{"answers":{"sev":{"type":"score","score":null,"confidence":0.8,"probabilities":{"0":0.5,"1":0.5}}}}`,
		"bad prob sum":         `{"answers":{"sev":{"type":"score","score":0.5,"confidence":0.8,"probabilities":{"0":0.9,"1":0.9}}}}`,
		"missing index":        `{"answers":{"sev":{"type":"score","score":0.5,"confidence":0.8,"probabilities":{"0":1.0}}}}`,
		"bad legend":           `{"answers":{"sev":{"type":"score","score":0.5,"confidence":0.8,"legend":{"0":"Low"},"probabilities":{"0":0.5,"1":0.5}}}}`,
	}
	for name, body := range mapScoreBad {
		t.Run("map_score/"+name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			for _, p := range []Provider{ProviderTypeSafe, ProviderGemini} {
				if _, err := (Client{Provider: p, APIKey: "k", Endpoint: srv.URL}).Decide(context.Background(), scoreReq); err == nil {
					t.Fatalf("%s accepted invalid score response (%s)", p, name)
				}
			}
		})
	}
}

func TestGeminiSafetyBlockClassificationWithoutEchoingMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"prohibited_content","message":"sensitive-prompt-fragment-do-not-echo"}}`)
	}))
	defer srv.Close()

	req, _ := BuildRequest("input", "", Checks{"safe": "Is it safe?"})
	_, err := (Client{Provider: ProviderGemini, APIKey: "secret-gemini-key", Endpoint: srv.URL}).Decide(context.Background(), req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrSafetyBlocked || apiErr.SafetyCode != "prohibited_content" {
		t.Fatalf("expected ErrSafetyBlocked with prohibited_content, got %#v (%v)", apiErr, err)
	}
	if strings.Contains(err.Error(), "sensitive-prompt-fragment") || strings.Contains(err.Error(), "secret-gemini-key") {
		t.Fatalf("leaked sensitive text in error: %v", err)
	}
}

func TestOpenRouterEndpointNormalizationAndValidation(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "https://openrouter.ai/api/alpha/decisions", false},
		{"https://openrouter.ai", "https://openrouter.ai/api/alpha/decisions", false},
		{"https://openrouter.ai/api/", "https://openrouter.ai/api/alpha/decisions", false},
		{"https://openrouter.ai/api/alpha/decisions", "https://openrouter.ai/api/alpha/decisions", false},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/systemone", false},
		{"https://openrouter.ai/api/v1/systemone", "https://openrouter.ai/api/v1/systemone", false},
		{"https://proxy.example.com/tenant%2Fname/api/", "https://proxy.example.com/tenant%2Fname/api/alpha/decisions", false},
		{"https://proxy.example.com/tenant%2Fname/api/alpha/decisions", "https://proxy.example.com/tenant%2Fname/api/alpha/decisions", false},
		{"ftp://openrouter.ai", "", true},
		{"https://user:secret@openrouter.ai/api/alpha/decisions", "", true},
		{"https://openrouter.ai/api/alpha/decisions?key=secret", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveOpenRouterEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ResolveOpenRouterEndpoint(%q) succeeded with %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ResolveOpenRouterEndpoint(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}
}

func TestLiteLLMEndpointNormalizationAndValidation(t *testing.T) {
	cases := []struct {
		in        string
		want      string
		systemOne bool
		wantErr   bool
	}{
		{"http://127.0.0.1:4000", "http://127.0.0.1:4000/v1/decisions", false, false},
		{"http://127.0.0.1:4000/", "http://127.0.0.1:4000/v1/decisions", false, false},
		{"https://gateway.example.com/v1", "https://gateway.example.com/v1/decisions", false, false},
		{"https://gateway.example.com/proxy/v1/", "https://gateway.example.com/proxy/v1/decisions", false, false},
		{"https://gateway.example.com/v1/decisions", "https://gateway.example.com/v1/decisions", false, false},
		{"https://gateway.example.com/decisions", "https://gateway.example.com/decisions", false, false},
		{"https://gateway.example.com/v1/systemone", "https://gateway.example.com/v1/systemone", true, false},
		{"https://gateway.example.com/systemone", "https://gateway.example.com/systemone", true, false},
		{"https://gateway.example.com/tenant%2Fname/v1/", "https://gateway.example.com/tenant%2Fname/v1/decisions", false, false},
		{"https://gateway.example.com/tenant%2Fname/v1/decisions", "https://gateway.example.com/tenant%2Fname/v1/decisions", false, false},
		{"", "", false, true},
		{"ftp://gateway.example.com", "", false, true},
		{"https://user:secret@gateway.example.com", "", false, true},
		{"https://gateway.example.com/v1/decisions?key=secret", "", false, true},
		{"https://gateway.example.com/v1/decisions#frag", "", false, true},
		{"/v1/decisions", "", false, true},
	}
	for _, tc := range cases {
		got, sysOne, err := ResolveLiteLLMEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ResolveLiteLLMEndpoint(%q) succeeded with %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want || sysOne != tc.systemOne {
			t.Fatalf("ResolveLiteLLMEndpoint(%q) = (%q, %v, %v), want (%q, %v, nil)", tc.in, got, sysOne, err, tc.want, tc.systemOne)
		}
	}
}

func TestBoundedResponseHandlingAcrossProviders(t *testing.T) {
	oversized := append([]byte(`{"answers":{"check":{"type":"noul","noul":0.5,"confidence":0.5}},"pad":"`), bytes.Repeat([]byte("a"), maxResponseBytes)...)
	oversized = append(oversized, []byte(`"}`)...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(oversized)
	}))
	defer srv.Close()

	req, _ := BuildRequest("input", "", Checks{"check": "Check?"})
	for _, p := range []Provider{ProviderOpenAI, ProviderTypeSafe, ProviderGemini, ProviderOpenRouter, ProviderLiteLLM} {
		_, err := (Client{
			Provider: p,
			Model:    "test-model",
			APIKey:   "test-key",
			Endpoint: srv.URL + "/v1/decisions",
		}).Decide(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
			t.Fatalf("provider %s did not enforce 4 MiB cap: %v", p, err)
		}
	}
}

func TestTimeoutRedirectAndHTTPStatusesAcrossProviders(t *testing.T) {
	req, _ := BuildRequest("input", "", Checks{"check": "Check?"})

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 529, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if status == http.StatusTemporaryRedirect {
					http.Redirect(w, r, "/other", status)
					return
				}
				w.WriteHeader(status)
				io.WriteString(w, "raw-sensitive-upstream-body-and-secret-token")
			}))
			defer srv.Close()

			for _, p := range []Provider{ProviderOpenAI, ProviderTypeSafe, ProviderGemini, ProviderOpenRouter, ProviderLiteLLM} {
				calls = 0
				_, err := (Client{
					Provider: p,
					Model:    "test-model",
					APIKey:   "secret-token",
					Endpoint: srv.URL + "/v1/decisions",
				}).Decide(context.Background(), req)
				if err == nil {
					t.Fatalf("%s accepted HTTP %d", p, status)
				}
				if calls != 1 {
					t.Fatalf("%s made %d calls (expected no redirect/retry)", p, calls)
				}
				if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "raw-sensitive") {
					t.Fatalf("%s leaked sensitive data in error: %v", p, err)
				}
			}
		})
	}

	t.Run("deadline_exceeded", func(t *testing.T) {
		unblock := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-unblock
		}))
		defer srv.Close()
		defer close(unblock)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		_, err := (Client{Provider: ProviderGemini, APIKey: "k", Endpoint: srv.URL}).Decide(ctx, req)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
	})
}

// TestLiveSmokeOptIn runs only when AID_LIVE_DECISIONS_TEST=1 is explicitly set by the operator.
func TestLiveSmokeOptIn(t *testing.T) {
	if os.Getenv("AID_LIVE_DECISIONS_TEST") != "1" {
		t.Skip("skipping live Decisions API smoke test; set AID_LIVE_DECISIONS_TEST=1 to run")
	}
	req, err := BuildRequest("diff --git a/x.go b/x.go\n+return 1\n", "", Checks{"changes_behavior": "Does this change behavior?"})
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, info := range Providers() {
		if info.RequiresURL {
			continue
		}
		key := strings.TrimSpace(os.Getenv(info.APIKeyEnv))
		if key == "" {
			continue
		}
		ran++
		t.Run(string(info.ID), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			answers, err := (Client{Provider: info.ID, APIKey: key}).Decide(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if answers["changes_behavior"].Probability == nil {
				t.Fatalf("missing probability: %+v", answers)
			}
		})
	}
	if ran == 0 {
		t.Skip("no provider API keys present in environment")
	}
}

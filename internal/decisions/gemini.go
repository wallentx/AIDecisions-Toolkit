package decisions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	GeminiDefaultModel = "gemini-decision-1"
	GeminiEndpoint     = "https://generativelanguage.googleapis.com/v1beta/decisions"
	geminiDisplayName  = "Gemini Decisions API"
	geminiMaxQuestions = 1000
	geminiMaxOptionLen = 128
	geminiMaxOptionUTF = 512
)

var geminiQuestionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type geminiAdapter struct{}

type geminiWireRequest struct {
	Model     string                        `json:"model"`
	Input     string                        `json:"input"`
	Questions map[string]geminiWireQuestion `json:"questions"`
}

type geminiWireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type geminiNoulCriteria struct {
	TrueDescription  string `json:"true_description,omitempty"`
	FalseDescription string `json:"false_description,omitempty"`
}

func (geminiAdapter) resolveTarget(model, endpoint string) (string, string, error) {
	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		resolvedModel = GeminiDefaultModel
	}
	resolvedEndpoint := strings.TrimSpace(endpoint)
	if resolvedEndpoint == "" {
		resolvedEndpoint = GeminiEndpoint
	}
	if _, err := ValidateEndpoint(resolvedEndpoint); err != nil {
		return "", "", err
	}
	return resolvedModel, resolvedEndpoint, nil
}

func (a geminiAdapter) preview(req Request, defaultModel, endpoint string) (Preview, error) {
	if err := ValidateRequest(req); err != nil {
		return Preview{}, err
	}
	reqModel := req.Model
	if strings.TrimSpace(reqModel) == "" {
		reqModel = defaultModel
	}
	model, targetURL, err := a.resolveTarget(reqModel, endpoint)
	if err != nil {
		return Preview{}, err
	}
	body, _, err := encodeGeminiWireRequest(model, req)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		Provider: ProviderGemini,
		Endpoint: targetURL,
		Model:    model,
		Request:  body,
	}, nil
}

func (a geminiAdapter) decide(ctx context.Context, c Client, req Request) (Response, error) {
	if err := ValidateRequest(req); err != nil {
		return Response{}, err
	}
	reqModel := req.Model
	if strings.TrimSpace(reqModel) == "" {
		reqModel = c.Model
	}
	model, targetURL, err := a.resolveTarget(reqModel, c.Endpoint)
	if err != nil {
		return Response{}, err
	}
	body, choiceMaps, err := encodeGeminiWireRequest(model, req)
	if err != nil {
		return Response{}, err
	}
	if err := ValidateAPIKey(c.APIKey, ProviderGemini); err != nil {
		return Response{}, err
	}
	headers := map[string]string{"x-goog-api-key": c.APIKey}
	respBytes, err := postJSON(ctx, ProviderGemini, geminiDisplayName, targetURL, headers, body, c.HTTP, classifyGeminiHTTPError)
	if err != nil {
		return Response{}, err
	}
	return decodeSystemOneWireResponse(ProviderGemini, geminiDisplayName, req, choiceMaps, true, false, respBytes)
}

func encodeGeminiWireRequest(model string, req Request) ([]byte, map[string]map[string]ChoiceValue, error) {
	if len(req.Questions) > geminiMaxQuestions {
		return nil, nil, &UnsupportedError{
			Provider: ProviderGemini,
			Feature:  fmt.Sprintf("more than %d questions per request (got %d)", geminiMaxQuestions, len(req.Questions)),
		}
	}
	questions := make(map[string]geminiWireQuestion, len(req.Questions))
	choiceMaps := make(map[string]map[string]ChoiceValue)
	for _, q := range req.Questions {
		if !geminiQuestionKeyPattern.MatchString(q.Name) {
			return nil, nil, &UnsupportedError{
				Provider: ProviderGemini,
				Feature:  fmt.Sprintf("question name %q outside [A-Za-z0-9_-]{1,64}", q.Name),
			}
		}
		kind, err := canonicalQuestionType(q.Type)
		if err != nil {
			return nil, nil, err
		}
		wq := geminiWireQuestion{Instructions: q.Instructions}
		switch kind {
		case QuestionPredicate:
			wq.Type = "noul"
			if q.Criteria != nil {
				crit := geminiNoulCriteria{
					TrueDescription:  strings.TrimSpace(q.Criteria.TrueDescription),
					FalseDescription: strings.TrimSpace(q.Criteria.FalseDescription),
				}
				if crit.TrueDescription != "" || crit.FalseDescription != "" {
					wq.Criteria = crit
				}
			}
		case QuestionChoice:
			wq.Type = "choice"
			criteria, byKey, err := buildStringKeyedChoices(ProviderGemini, q)
			if err != nil {
				return nil, nil, err
			}
			for key := range criteria {
				if err := validateGeminiOptionKey(q.Name, key); err != nil {
					return nil, nil, err
				}
			}
			wq.Criteria = criteria
			choiceMaps[q.Name] = byKey
		case QuestionScore:
			wq.Type = "score"
			levels := make([]string, len(q.Levels))
			for i, lvl := range q.Levels {
				levels[i] = formatScoreCriterion(lvl)
			}
			wq.Criteria = levels
		}
		questions[q.Name] = wq
	}
	wire := geminiWireRequest{
		Model:     model,
		Input:     req.Input,
		Questions: questions,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, err
	}
	return body, choiceMaps, nil
}

func validateGeminiOptionKey(questionName, key string) error {
	runes := utf8.RuneCountInString(key)
	if runes < 1 || runes > geminiMaxOptionLen || len(key) > geminiMaxOptionUTF {
		return &UnsupportedError{
			Provider: ProviderGemini,
			Feature:  fmt.Sprintf("choice option key length on question %q outside 1..128 characters / 512 UTF-8 bytes", questionName),
		}
	}
	if key[0] == ' ' || key[len(key)-1] == ' ' {
		return &UnsupportedError{
			Provider: ProviderGemini,
			Feature:  fmt.Sprintf("choice option key with leading or trailing spaces on question %q", questionName),
		}
	}
	for _, r := range key {
		if unicode.IsControl(r) || (unicode.IsSpace(r) && r != ' ') {
			return &UnsupportedError{
				Provider: ProviderGemini,
				Feature:  fmt.Sprintf("choice option key containing control or non-ASCII space characters on question %q", questionName),
			}
		}
	}
	return nil
}

// classifyGeminiHTTPError recognizes documented safety-filter HTTP 400 codes
// without echoing untrusted response error messages.
func classifyGeminiHTTPError(statusCode int, body []byte) *APIError {
	if statusCode != http.StatusBadRequest {
		return nil
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	switch envelope.Error.Code {
	case "safety", "prohibited_content", "blocklist", "image_safety", "content_blocked":
		return &APIError{
			Provider:   ProviderGemini,
			Kind:       ErrSafetyBlocked,
			StatusCode: statusCode,
			SafetyCode: envelope.Error.Code,
			Message:    fmt.Sprintf("%s returned HTTP 400 (request blocked by safety filter: %s)", geminiDisplayName, envelope.Error.Code),
		}
	default:
		return nil
	}
}

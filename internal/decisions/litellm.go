package decisions

import (
	"context"
	"errors"
	"strings"
)

const (
	liteLLMDisplayName  = "LiteLLM Decisions API"
	liteLLMMaxQuestions = 128
)

type liteLLMAdapter struct{}

// ResolveLiteLLMEndpoint validates a configured LiteLLM URL and normalizes base URLs
// onto /v1/decisions while preserving explicit /v1/decisions, /decisions, or /v1/systemone paths.
func ResolveLiteLLMEndpoint(rawURL string) (endpoint string, systemOne bool, err error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", false, errors.New("litellm requires an explicit endpoint URL (set --endpoint or configure providers.litellm.endpoint)")
	}
	parsed, err := ValidateEndpoint(trimmed)
	if err != nil {
		return "", false, err
	}
	cleanPath := strings.TrimRight(parsed.Path, "/")
	switch {
	case cleanPath == "":
		parsed.Path = "/v1/decisions"
	case cleanPath == "/v1" || strings.HasSuffix(cleanPath, "/v1"):
		parsed.Path = cleanPath + "/decisions"
	case cleanPath == "/systemone" || strings.HasSuffix(cleanPath, "/systemone"):
		parsed.Path = cleanPath
		systemOne = true
	default:
		parsed.Path = cleanPath
	}
	parsed.RawPath = ""
	return parsed.String(), systemOne, nil
}

func (liteLLMAdapter) resolveTarget(model, endpoint string) (string, string, bool, error) {
	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		return "", "", false, errors.New("litellm requires an explicit model name (set --model or configure providers.litellm.model)")
	}
	targetURL, systemOne, err := ResolveLiteLLMEndpoint(endpoint)
	if err != nil {
		return "", "", false, err
	}
	return resolvedModel, targetURL, systemOne, nil
}

func (a liteLLMAdapter) preview(req Request, defaultModel, endpoint string) (Preview, error) {
	if err := ValidateRequest(req); err != nil {
		return Preview{}, err
	}
	reqModel := req.Model
	if strings.TrimSpace(reqModel) == "" {
		reqModel = defaultModel
	}
	model, targetURL, systemOne, err := a.resolveTarget(reqModel, endpoint)
	if err != nil {
		return Preview{}, err
	}
	var body []byte
	if systemOne {
		body, _, err = encodeSystemOneWireRequest(ProviderLiteLLM, model, req, liteLLMMaxQuestions)
	} else {
		body, err = encodeOpenAIWireRequest(ProviderLiteLLM, model, req, liteLLMMaxQuestions, true)
	}
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		Provider: ProviderLiteLLM,
		Endpoint: targetURL,
		Model:    model,
		Request:  body,
	}, nil
}

func (a liteLLMAdapter) decide(ctx context.Context, c Client, req Request) (Response, error) {
	if err := ValidateRequest(req); err != nil {
		return Response{}, err
	}
	reqModel := req.Model
	if strings.TrimSpace(reqModel) == "" {
		reqModel = c.Model
	}
	model, targetURL, systemOne, err := a.resolveTarget(reqModel, c.Endpoint)
	if err != nil {
		return Response{}, err
	}
	var (
		body       []byte
		choiceMaps map[string]map[string]ChoiceValue
	)
	if systemOne {
		body, choiceMaps, err = encodeSystemOneWireRequest(ProviderLiteLLM, model, req, liteLLMMaxQuestions)
	} else {
		body, err = encodeOpenAIWireRequest(ProviderLiteLLM, model, req, liteLLMMaxQuestions, true)
	}
	if err != nil {
		return Response{}, err
	}
	if err := ValidateAPIKey(c.APIKey, ProviderLiteLLM); err != nil {
		return Response{}, err
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	respBytes, err := postJSON(ctx, ProviderLiteLLM, liteLLMDisplayName, targetURL, headers, body, c.HTTP, nil)
	if err != nil {
		return Response{}, err
	}
	if systemOne {
		return decodeSystemOneWireResponse(ProviderLiteLLM, liteLLMDisplayName, req, choiceMaps, false, respBytes)
	}
	return decodeOpenAIWireResponse(ProviderLiteLLM, liteLLMDisplayName, req, respBytes)
}

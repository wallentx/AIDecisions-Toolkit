package decisions

import (
	"context"
	"strings"
)

const (
	OpenRouterDefaultModel = "~typesafe/jev-latest"
	OpenRouterEndpoint     = "https://openrouter.ai/api/alpha/decisions"
	openRouterDisplayName  = "OpenRouter Decisions API"
)

type openRouterAdapter struct{}

// ResolveOpenRouterEndpoint validates an OpenRouter Decisions URL, defaulting to
// https://openrouter.ai/api/alpha/decisions and normalizing bare /api or /v1 base URLs
// while preserving explicit /api/alpha/decisions or /api/v1/systemone paths.
func ResolveOpenRouterEndpoint(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		trimmed = OpenRouterEndpoint
	}
	parsed, err := ValidateEndpoint(trimmed)
	if err != nil {
		return "", err
	}
	cleanEscaped := strings.TrimRight(parsed.EscapedPath(), "/")
	var targetEscaped string
	switch {
	case cleanEscaped == "":
		targetEscaped = "/api/alpha/decisions"
	case cleanEscaped == "/api" || strings.HasSuffix(cleanEscaped, "/api"):
		targetEscaped = cleanEscaped + "/alpha/decisions"
	case cleanEscaped == "/v1" || strings.HasSuffix(cleanEscaped, "/v1"):
		targetEscaped = cleanEscaped + "/systemone"
	default:
		targetEscaped = cleanEscaped
	}
	if err := setEscapedEndpointPath(parsed, targetEscaped); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func (openRouterAdapter) resolveTarget(model, endpoint string) (string, string, error) {
	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		resolvedModel = OpenRouterDefaultModel
	}
	resolvedEndpoint, err := ResolveOpenRouterEndpoint(endpoint)
	if err != nil {
		return "", "", err
	}
	return resolvedModel, resolvedEndpoint, nil
}

func (a openRouterAdapter) preview(req Request, defaultModel, endpoint string) (Preview, error) {
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
	body, _, err := encodeSystemOneWireRequest(ProviderOpenRouter, model, req, 0, maxScoreLevels)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		Provider: ProviderOpenRouter,
		Endpoint: targetURL,
		Model:    model,
		Request:  body,
	}, nil
}

func (a openRouterAdapter) decide(ctx context.Context, c Client, req Request) (Response, error) {
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
	body, choiceMaps, err := encodeSystemOneWireRequest(ProviderOpenRouter, model, req, 0, maxScoreLevels)
	if err != nil {
		return Response{}, err
	}
	if err := ValidateAPIKey(c.APIKey, ProviderOpenRouter); err != nil {
		return Response{}, err
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	respBytes, err := postJSON(ctx, ProviderOpenRouter, openRouterDisplayName, targetURL, headers, body, c.HTTP, nil)
	if err != nil {
		return Response{}, err
	}
	return decodeSystemOneWireResponse(ProviderOpenRouter, openRouterDisplayName, req, choiceMaps, false, true, respBytes)
}

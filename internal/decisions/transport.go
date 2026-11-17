package decisions

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

const maxResponseBytes = 4 << 20

type statusErrorClassifier func(statusCode int, body []byte) *APIError

func postJSON(
	ctx context.Context,
	provider Provider,
	displayName string,
	endpoint string,
	headers map[string]string,
	payload []byte,
	transport *http.Client,
	classify statusErrorClassifier,
) ([]byte, error) {
	if _, err := ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &APIError{
			Provider: provider,
			Kind:     ErrTransport,
			Message:  fmt.Sprintf("%s request creation failed", displayName),
			Cause:    err,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	baseClient := http.DefaultClient
	if transport != nil {
		baseClient = transport
	}
	httpClient := *baseClient
	// Never forward credential-bearing requests across HTTP redirects.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Sanitize net/url.Error so full URLs or embedded parameters cannot leak,
		// while preserving context cancellation/deadline through Cause.
		cause := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause = ctxErr
		}
		return nil, &APIError{
			Provider: provider,
			Kind:     ErrTransport,
			Message:  fmt.Sprintf("%s request failed: %v", displayName, safeTransportReason(err)),
			Cause:    cause,
		}
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &APIError{
			Provider:   provider,
			Kind:       ErrRedirect,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (redirects are refused for credentialed requests)", displayName, resp.StatusCode),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if classify != nil {
			if custom := classify(resp.StatusCode, data); custom != nil {
				return nil, custom
			}
		}
		return nil, defaultHTTPError(provider, displayName, resp.StatusCode)
	}
	if readErr != nil {
		return nil, &APIError{
			Provider: provider,
			Kind:     ErrTransport,
			Message:  fmt.Sprintf("read %s response failed", displayName),
			Cause:    readErr,
		}
	}
	if len(data) > maxResponseBytes {
		return nil, &APIError{
			Provider: provider,
			Kind:     ErrProtocol,
			Message:  fmt.Sprintf("%s response exceeds 4 MiB", displayName),
		}
	}
	return data, nil
}

func safeTransportReason(err error) string {
	switch {
	case err == nil:
		return "unknown transport error"
	case bytes.Contains([]byte(err.Error()), []byte("context deadline exceeded")):
		return "context deadline exceeded"
	case bytes.Contains([]byte(err.Error()), []byte("context canceled")):
		return "context canceled"
	default:
		return "network or connection error"
	}
}

func defaultHTTPError(provider Provider, displayName string, statusCode int) *APIError {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &APIError{
			Provider:   provider,
			Kind:       ErrAuthentication,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (check your API key, access, or permissions)", displayName, statusCode),
		}
	case http.StatusTooManyRequests:
		return &APIError{
			Provider:   provider,
			Kind:       ErrRateLimited,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (rate limit or quota exceeded)", displayName, statusCode),
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		return &APIError{
			Provider:   provider,
			Kind:       ErrBadRequest,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (request rejected by provider validation)", displayName, statusCode),
		}
	case http.StatusServiceUnavailable, 529:
		return &APIError{
			Provider:   provider,
			Kind:       ErrOverloaded,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (provider service overloaded or unavailable)", displayName, statusCode),
		}
	default:
		return &APIError{
			Provider:   provider,
			Kind:       ErrHTTP,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("%s returned HTTP %d (check your API key, access, or quota)", displayName, statusCode),
		}
	}
}

func protocolError(provider Provider, displayName, detail string) error {
	return &APIError{
		Provider: provider,
		Kind:     ErrProtocol,
		Message:  fmt.Sprintf("%s: %s", displayName, detail),
	}
}

package decisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
)

const (
	maxChoices     = 255
	maxScoreLevels = 255
	probSumEpsilon = 0.05
	scoreEpsilon   = 1e-6
)

// canonicalQuestionType normalizes QuestionNoul and QuestionPredicate to QuestionPredicate.
func canonicalQuestionType(kind QuestionType) (QuestionType, error) {
	switch kind {
	case QuestionPredicate, QuestionNoul:
		return QuestionPredicate, nil
	case QuestionChoice:
		return QuestionChoice, nil
	case QuestionScore:
		return QuestionScore, nil
	case "":
		return "", errors.New("question type must be predicate, choice, or score")
	default:
		return "", &UnsupportedError{Feature: fmt.Sprintf("question type %q", kind)}
	}
}

// ValidateRequest checks provider-independent structural invariants on a canonical Request.
func ValidateRequest(req Request) error {
	if strings.TrimSpace(req.Input) == "" {
		return errors.New("provide nonempty evidence")
	}
	if len(req.Questions) == 0 {
		return errors.New("provide at least one check")
	}
	seenNames := make(map[string]bool, len(req.Questions))
	for _, q := range req.Questions {
		if strings.TrimSpace(q.Name) == "" {
			return errors.New("check names and instructions must be nonempty strings")
		}
		if strings.TrimSpace(q.Name) != q.Name {
			return fmt.Errorf("question name %q must not have leading or trailing whitespace", q.Name)
		}
		if seenNames[q.Name] {
			return fmt.Errorf("duplicate question name %q", q.Name)
		}
		seenNames[q.Name] = true
		if strings.TrimSpace(q.Instructions) == "" {
			return errors.New("check names and instructions must be nonempty strings")
		}
		kind, err := canonicalQuestionType(q.Type)
		if err != nil {
			return err
		}
		switch kind {
		case QuestionPredicate:
			if len(q.Choices) != 0 || len(q.Levels) != 0 {
				return fmt.Errorf("predicate question %q cannot declare choices or score levels", q.Name)
			}
			if q.Criteria != nil && strings.TrimSpace(q.Criteria.TrueDescription) == "" && strings.TrimSpace(q.Criteria.FalseDescription) == "" {
				return fmt.Errorf("predicate question %q criteria must include a true or false description", q.Name)
			}
		case QuestionChoice:
			if q.Criteria != nil || len(q.Levels) != 0 {
				return fmt.Errorf("choice question %q cannot declare predicate criteria or score levels", q.Name)
			}
			if len(q.Choices) < 2 || len(q.Choices) > maxChoices {
				return fmt.Errorf("choice question %q must declare between 2 and %d choices", q.Name, maxChoices)
			}
			seenValues := make(map[ChoiceValue]bool, len(q.Choices))
			for _, opt := range q.Choices {
				if !opt.Value.IsValid() {
					return fmt.Errorf("choice question %q has an unset option value", q.Name)
				}
				if s, ok := opt.Value.StringValue(); ok && strings.TrimSpace(s) == "" {
					return fmt.Errorf("choice question %q has an empty string option value", q.Name)
				}
				if seenValues[opt.Value] {
					return fmt.Errorf("choice question %q has duplicate option values", q.Name)
				}
				seenValues[opt.Value] = true
			}
		case QuestionScore:
			if q.Criteria != nil || len(q.Choices) != 0 {
				return fmt.Errorf("score question %q cannot declare predicate criteria or choices", q.Name)
			}
			if len(q.Levels) < 2 || len(q.Levels) > maxScoreLevels {
				return fmt.Errorf("score question %q must declare between 2 and %d levels", q.Name, maxScoreLevels)
			}
			for _, level := range q.Levels {
				if strings.TrimSpace(level.Label) == "" {
					return fmt.Errorf("score question %q has an empty level label", q.Name)
				}
			}
		}
	}
	return nil
}

// ValidateAPIKey ensures an API key is nonempty printable ASCII without whitespace.
// It never includes key contents in error messages.
func ValidateAPIKey(key string, provider Provider) error {
	info, ok := LookupProvider(provider)
	envHint := "the provider API key"
	if ok && info.APIKeyEnv != "" {
		envHint = info.APIKeyEnv
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("set %s or configure a provider api_key_file before making an API request; --dry-run needs no key", envHint)
	}
	if strings.TrimSpace(key) != key {
		return errors.New("API key must contain only printable ASCII without whitespace")
	}
	for _, r := range key {
		if r <= ' ' || r >= 127 {
			return errors.New("API key must contain only printable ASCII without whitespace")
		}
	}
	return nil
}

// ValidateEndpoint checks that rawURL is a clean http or https endpoint without
// embedded credentials, query parameters, fragments, or control characters.
func ValidateEndpoint(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("endpoint URL must not be empty")
	}
	for _, r := range rawURL {
		if r <= ' ' || r >= 127 {
			return nil, errors.New("endpoint URL must contain printable ASCII without whitespace")
		}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("invalid endpoint URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("endpoint URL scheme must be https or http")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("endpoint URL must include a host")
	}
	if parsed.User != nil {
		return nil, errors.New("endpoint URL must not contain embedded credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, errors.New("endpoint URL must not contain query parameters")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("endpoint URL must not contain a URL fragment")
	}
	return parsed, nil
}

// setEscapedEndpointPath updates parsed with an escaped path, preserving both
// percent-encoded separators (such as %2F) and leading double slashes (such as //tenant).
func setEscapedEndpointPath(parsed *url.URL, escapedPath string) error {
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return errors.New("invalid endpoint URL path")
	}
	parsed.Path = decoded
	parsed.RawPath = escapedPath
	if parsed.EscapedPath() != escapedPath {
		return errors.New("invalid endpoint URL path")
	}
	return nil
}

func validProbability(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= 1
}

func validProbabilityValue(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1
}

func parseJSONFloat(raw json.RawMessage) (float64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, errors.New("expected JSON number")
	}
	var v float64
	if err := json.Unmarshal(trimmed, &v); err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("expected finite JSON number")
	}
	return v, nil
}

func parseJSONInt(raw json.RawMessage) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, errors.New("expected JSON integer")
	}
	var v int
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return 0, errors.New("expected JSON integer")
	}
	return v, nil
}

func parseJSONString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", errors.New("expected JSON string")
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", err
	}
	return s, nil
}

func validProbabilitySum(sum float64) bool {
	return !math.IsNaN(sum) && !math.IsInf(sum, 0) && math.Abs(sum-1.0) <= probSumEpsilon
}

func validScore(score *float64, levels int) bool {
	if score == nil || math.IsNaN(*score) || math.IsInf(*score, 0) {
		return false
	}
	maxVal := float64(levels - 1)
	return *score >= -scoreEpsilon && *score <= maxVal+scoreEpsilon
}

func clampScoreZero(v float64, levels int) float64 {
	maxVal := float64(levels - 1)
	if v < 0 && v >= -scoreEpsilon {
		return 0
	}
	if v > maxVal && v <= maxVal+scoreEpsilon {
		return maxVal
	}
	return v
}

func formatScoreCriterion(level ScoreLevel) string {
	label := strings.TrimSpace(level.Label)
	desc := strings.TrimSpace(level.Description)
	if desc == "" {
		return label
	}
	return label + ": " + desc
}

// buildStringKeyedChoices converts ChoiceOption values into string-keyed criteria
// for System One and Gemini protocols while rejecting string/bool collisions.
func buildStringKeyedChoices(provider Provider, q Question) (map[string]string, map[string]ChoiceValue, error) {
	criteria := make(map[string]string, len(q.Choices))
	byKey := make(map[string]ChoiceValue, len(q.Choices))
	for _, opt := range q.Choices {
		key, err := opt.Value.Key()
		if err != nil {
			return nil, nil, err
		}
		if _, exists := byKey[key]; exists {
			return nil, nil, &UnsupportedError{
				Provider: provider,
				Feature:  fmt.Sprintf("choice question %q with both boolean and string %q options", q.Name, key),
			}
		}
		criteria[key] = opt.Description
		byKey[key] = opt.Value
	}
	return criteria, byKey, nil
}

// decodeUniqueObject parses a JSON object while rejecting duplicate keys.
func decodeUniqueObject(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("expected string key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate JSON key %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	endTok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if endDelim, ok := endTok.(json.Delim); !ok || endDelim != '}' {
		return nil, errors.New("malformed JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unexpected trailing JSON content")
	}
	return fields, nil
}

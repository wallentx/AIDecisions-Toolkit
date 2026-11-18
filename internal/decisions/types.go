// Package decisions evaluates typed decision questions across native Decisions APIs
// and optional LiteLLM gateways through a single canonical representation.
package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Provider identifies a Decisions API backend.
type Provider string

const (
	ProviderOpenAI     Provider = "openai"
	ProviderTypeSafe   Provider = "typesafe"
	ProviderGemini     Provider = "gemini"
	ProviderOpenRouter Provider = "openrouter"
	ProviderLiteLLM    Provider = "litellm"

	DefaultProvider = ProviderOpenAI
)

// ProviderInfo describes a registered provider's defaults and credential contract.
type ProviderInfo struct {
	ID              Provider
	DisplayName     string
	DefaultModel    string
	DefaultEndpoint string
	APIKeyEnv       string
	RequiresModel   bool
	RequiresURL     bool
}

// QuestionType identifies a canonical question kind.
type QuestionType string

const (
	QuestionPredicate QuestionType = "predicate"
	QuestionNoul      QuestionType = "noul"
	QuestionChoice    QuestionType = "choice"
	QuestionScore     QuestionType = "score"
)

// AnswerType identifies a normalized answer kind.
type AnswerType string

const (
	AnswerPredicate AnswerType = "predicate"
	AnswerChoice    AnswerType = "choice"
	AnswerScore     AnswerType = "score"
	AnswerRefusal   AnswerType = "refusal"
)

// Checks maps stable question names to yes/no predicate questions.
type Checks map[string]string

type choiceKind uint8

const (
	choiceUnset choiceKind = iota
	choiceString
	choiceBool
)

// ChoiceValue holds a typed option value (string or boolean).
// A string such as "true" and a boolean true remain distinct values.
type ChoiceValue struct {
	kind    choiceKind
	strVal  string
	boolVal bool
}

// StringChoice constructs a string choice option value.
func StringChoice(v string) ChoiceValue {
	return ChoiceValue{kind: choiceString, strVal: v}
}

// BoolChoice constructs a boolean choice option value.
func BoolChoice(v bool) ChoiceValue {
	return ChoiceValue{kind: choiceBool, boolVal: v}
}

// IsValid reports whether v holds an initialized string or boolean value.
func (v ChoiceValue) IsValid() bool {
	return v.kind == choiceString || v.kind == choiceBool
}

// IsBool reports whether v holds a boolean value.
func (v ChoiceValue) IsBool() bool { return v.kind == choiceBool }

// IsString reports whether v holds a string value.
func (v ChoiceValue) IsString() bool { return v.kind == choiceString }

// Bool returns the boolean value when IsBool is true.
func (v ChoiceValue) Bool() (bool, bool) { return v.boolVal, v.kind == choiceBool }

// StringValue returns the string value when IsString is true.
func (v ChoiceValue) StringValue() (string, bool) { return v.strVal, v.kind == choiceString }

// Key returns the string representation used by providers that key choices by text.
func (v ChoiceValue) Key() (string, error) {
	switch v.kind {
	case choiceString:
		return v.strVal, nil
	case choiceBool:
		if v.boolVal {
			return "true", nil
		}
		return "false", nil
	default:
		return "", errors.New("choice value is unset")
	}
}

func (v ChoiceValue) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case choiceString:
		return json.Marshal(v.strVal)
	case choiceBool:
		return json.Marshal(v.boolVal)
	default:
		return nil, errors.New("choice value must be a string or boolean")
	}
}

func (v *ChoiceValue) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("choice value must be a string or boolean")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*v = StringChoice(s)
		return nil
	}
	if bytes.Equal(trimmed, []byte("true")) {
		*v = BoolChoice(true)
		return nil
	}
	if bytes.Equal(trimmed, []byte("false")) {
		*v = BoolChoice(false)
		return nil
	}
	return errors.New("choice value must be a string or boolean")
}

// ChoiceOption defines one selectable alternative in a choice question.
type ChoiceOption struct {
	Value       ChoiceValue `json:"value"`
	Description string      `json:"description,omitempty"`
}

// ScoreLevel defines one ordered rubric level starting at index 0.
type ScoreLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PredicateCriteria defines optional true/false boundary descriptions for yes/no questions.
type PredicateCriteria struct {
	TrueDescription  string `json:"true_description,omitempty"`
	FalseDescription string `json:"false_description,omitempty"`
}

// Question defines one named decision question evaluated against shared input.
type Question struct {
	Type         QuestionType       `json:"type"`
	Name         string             `json:"name"`
	Instructions string             `json:"instructions"`
	Criteria     *PredicateCriteria `json:"criteria,omitempty"`
	Choices      []ChoiceOption     `json:"choices,omitempty"`
	Levels       []ScoreLevel       `json:"levels,omitempty"`
}

// Request is the provider-independent Decisions evaluation request.
type Request struct {
	Model     string     `json:"model,omitempty"`
	Input     string     `json:"input"`
	Questions []Question `json:"questions"`
}

// ChoiceProbability pairs one declared choice option with its estimated probability.
type ChoiceProbability struct {
	Value       ChoiceValue `json:"value"`
	Probability float64     `json:"probability"`
}

// ScoreProbability pairs one rubric level index and label with its estimated probability.
type ScoreProbability struct {
	Index       int     `json:"value"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
}

// Answer is the normalized result for one named question.
// Predicate-only consumers continue to read Type, Name, and Probability.
type Answer struct {
	Type                AnswerType          `json:"type"`
	Name                string              `json:"name"`
	Probability         *float64            `json:"probability,omitempty"`
	Choice              *ChoiceValue        `json:"choice,omitempty"`
	ChoiceProbabilities []ChoiceProbability `json:"choice_probabilities,omitempty"`
	Score               *float64            `json:"score,omitempty"`
	ScoreProbabilities  []ScoreProbability  `json:"score_probabilities,omitempty"`
	Confidence          *float64            `json:"confidence,omitempty"`
	Provider            Provider            `json:"provider,omitempty"`
	Model               string              `json:"model,omitempty"`
}

// Response carries both named answers and question-ordered answers with provider metadata.
type Response struct {
	Provider Provider          `json:"provider"`
	Model    string            `json:"model,omitempty"`
	Answers  map[string]Answer `json:"answers"`
	Ordered  []Answer          `json:"ordered,omitempty"`
}

// Preview describes the resolved provider target and wire payload without credentials.
type Preview struct {
	Provider Provider        `json:"provider"`
	Endpoint string          `json:"endpoint"`
	Model    string          `json:"model"`
	Request  json.RawMessage `json:"request"`
}

// Evaluator evaluates canonical decision requests across any configured provider.
type Evaluator interface {
	Decide(ctx context.Context, req Request) (map[string]Answer, error)
}

// UnsupportedError reports a feature or constraint unsupported by the chosen provider.
type UnsupportedError struct {
	Provider Provider
	Feature  string
}

func (e *UnsupportedError) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("provider %s does not support %s", e.Provider, e.Feature)
	}
	return fmt.Sprintf("unsupported capability: %s", e.Feature)
}

// IsUnsupported reports whether err represents an unsupported capability.
func IsUnsupported(err error) bool {
	var target *UnsupportedError
	return errors.As(err, &target)
}

// ErrorKind classifies transport, authentication, safety, and protocol failures.
type ErrorKind string

const (
	ErrAuthentication ErrorKind = "authentication"
	ErrRateLimited    ErrorKind = "rate_limited"
	ErrSafetyBlocked  ErrorKind = "safety_blocked"
	ErrOverloaded     ErrorKind = "overloaded"
	ErrBadRequest     ErrorKind = "bad_request"
	ErrRedirect       ErrorKind = "redirect"
	ErrProtocol       ErrorKind = "protocol"
	ErrTransport      ErrorKind = "transport"
	ErrHTTP           ErrorKind = "http"
)

// APIError represents a sanitized provider communication or response error.
type APIError struct {
	Provider   Provider
	Kind       ErrorKind
	StatusCode int
	SafetyCode string
	Message    string
	Cause      error
}

func (e *APIError) Error() string { return e.Message }

func (e *APIError) Unwrap() error { return e.Cause }

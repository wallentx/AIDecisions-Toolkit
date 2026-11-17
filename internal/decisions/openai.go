package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	OpenAIDefaultModel   = "gpt-6-luna"
	OpenAIEndpoint       = "https://api.openai.com/v1/decisions"
	openAIDisplayName    = "OpenAI Decisions API"
	openAIMaxQuestions   = 400
	openAIMaxScoreLevels = 10
)

type openAIAdapter struct{}

type openAIWireRequest struct {
	Model     string               `json:"model"`
	Input     string               `json:"input"`
	Questions []openAIWireQuestion `json:"questions"`
}

type openAIWireQuestion struct {
	Type         string             `json:"type"`
	Name         string             `json:"name"`
	Instructions string             `json:"instructions"`
	Choices      []openAIWireChoice `json:"choices,omitempty"`
	Levels       []openAIWireLevel  `json:"levels,omitempty"`
}

type openAIWireChoice struct {
	Value       ChoiceValue `json:"value"`
	Description string      `json:"description,omitempty"`
}

type openAIWireLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func (openAIAdapter) resolveTarget(model, endpoint string) (string, string, error) {
	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		resolvedModel = OpenAIDefaultModel
	}
	resolvedEndpoint := strings.TrimSpace(endpoint)
	if resolvedEndpoint == "" {
		resolvedEndpoint = OpenAIEndpoint
	}
	if _, err := ValidateEndpoint(resolvedEndpoint); err != nil {
		return "", "", err
	}
	return resolvedModel, resolvedEndpoint, nil
}

func (a openAIAdapter) preview(req Request, defaultModel, endpoint string) (Preview, error) {
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
	body, err := encodeOpenAIWireRequest(ProviderOpenAI, model, req, openAIMaxQuestions, false)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		Provider: ProviderOpenAI,
		Endpoint: targetURL,
		Model:    model,
		Request:  body,
	}, nil
}

func (a openAIAdapter) decide(ctx context.Context, c Client, req Request) (Response, error) {
	preview, err := a.preview(req, c.Model, c.Endpoint)
	if err != nil {
		return Response{}, err
	}
	if err := ValidateAPIKey(c.APIKey, ProviderOpenAI); err != nil {
		return Response{}, err
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	respBytes, err := postJSON(ctx, ProviderOpenAI, openAIDisplayName, preview.Endpoint, headers, preview.Request, c.HTTP, nil)
	if err != nil {
		return Response{}, err
	}
	return decodeOpenAIWireResponse(ProviderOpenAI, openAIDisplayName, req, respBytes)
}

func encodeOpenAIWireRequest(
	provider Provider,
	model string,
	req Request,
	maxQuestions int,
	rejectChoiceKeyCollision bool,
) ([]byte, error) {
	if maxQuestions > 0 && len(req.Questions) > maxQuestions {
		return nil, &UnsupportedError{
			Provider: provider,
			Feature:  fmt.Sprintf("more than %d questions per request (got %d)", maxQuestions, len(req.Questions)),
		}
	}
	wire := openAIWireRequest{
		Model:     model,
		Input:     req.Input,
		Questions: make([]openAIWireQuestion, 0, len(req.Questions)),
	}
	for _, q := range req.Questions {
		kind, err := canonicalQuestionType(q.Type)
		if err != nil {
			return nil, err
		}
		wq := openAIWireQuestion{
			Name:         q.Name,
			Instructions: q.Instructions,
		}
		switch kind {
		case QuestionPredicate:
			if q.Criteria != nil && (strings.TrimSpace(q.Criteria.TrueDescription) != "" || strings.TrimSpace(q.Criteria.FalseDescription) != "") {
				return nil, &UnsupportedError{
					Provider: provider,
					Feature:  fmt.Sprintf("predicate true/false criteria descriptions on question %q", q.Name),
				}
			}
			wq.Type = "predicate"
		case QuestionChoice:
			if rejectChoiceKeyCollision {
				if _, _, err := buildStringKeyedChoices(provider, q); err != nil {
					return nil, err
				}
			}
			wq.Type = "choice"
			wq.Choices = make([]openAIWireChoice, len(q.Choices))
			for i, opt := range q.Choices {
				wq.Choices[i] = openAIWireChoice{
					Value:       opt.Value,
					Description: opt.Description,
				}
			}
		case QuestionScore:
			if len(q.Levels) > openAIMaxScoreLevels {
				return nil, &UnsupportedError{
					Provider: provider,
					Feature:  fmt.Sprintf("score questions with more than %d levels on question %q", openAIMaxScoreLevels, q.Name),
				}
			}
			wq.Type = "score"
			wq.Levels = make([]openAIWireLevel, len(q.Levels))
			for i, lvl := range q.Levels {
				wq.Levels[i] = openAIWireLevel{
					Label:       lvl.Label,
					Description: lvl.Description,
				}
			}
		}
		wire.Questions = append(wire.Questions, wq)
	}
	return json.Marshal(wire)
}

func decodeOpenAIWireResponse(provider Provider, displayName string, req Request, data []byte) (Response, error) {
	root, err := decodeUniqueObject(data)
	if err != nil {
		return Response{}, protocolError(provider, displayName, "returned invalid JSON")
	}
	var respModel string
	if rawModel, ok := root["model"]; ok && !bytes.Equal(bytes.TrimSpace(rawModel), []byte("null")) {
		s, err := parseJSONString(rawModel)
		if err != nil {
			return Response{}, protocolError(provider, displayName, "returned an invalid model field")
		}
		respModel = s
	}
	rawAnswers, ok := root["answers"]
	if !ok {
		return Response{}, protocolError(provider, displayName, "omitted answers")
	}
	var answerItems []json.RawMessage
	if err := json.Unmarshal(rawAnswers, &answerItems); err != nil || answerItems == nil {
		return Response{}, protocolError(provider, displayName, "returned invalid answers array")
	}

	questionsByName := make(map[string]Question, len(req.Questions))
	wanted := make(map[string]bool, len(req.Questions))
	for _, q := range req.Questions {
		questionsByName[q.Name] = q
		wanted[q.Name] = true
	}

	answers := make(map[string]Answer, len(req.Questions))
	for _, rawAns := range answerItems {
		fields, err := decodeUniqueObject(rawAns)
		if err != nil {
			return Response{}, protocolError(provider, displayName, "returned an invalid answer object")
		}
		rawName, hasName := fields["name"]
		name, nameErr := parseJSONString(rawName)
		if !hasName || nameErr != nil || name == "" {
			return Response{}, protocolError(provider, displayName, "returned an answer without a valid name")
		}
		if !wanted[name] {
			return Response{}, protocolError(provider, displayName, "returned an unexpected or duplicate answer name")
		}
		delete(wanted, name)

		rawType, hasType := fields["type"]
		ansType, typeErr := parseJSONString(rawType)
		if !hasType || typeErr != nil {
			return Response{}, protocolError(provider, displayName, "returned an answer without a valid type")
		}

		q := questionsByName[name]
		expectedKind, _ := canonicalQuestionType(q.Type)

		ans, err := normalizeOpenAIAnswer(provider, displayName, respModel, q, expectedKind, ansType, fields)
		if err != nil {
			return Response{}, err
		}
		answers[name] = ans
	}
	if len(wanted) != 0 {
		return Response{}, protocolError(provider, displayName, "omitted requested answers")
	}
	ordered := make([]Answer, len(req.Questions))
	for i, q := range req.Questions {
		ordered[i] = answers[q.Name]
	}
	return Response{
		Provider: provider,
		Model:    respModel,
		Answers:  answers,
		Ordered:  ordered,
	}, nil
}

func normalizeOpenAIAnswer(
	provider Provider,
	displayName string,
	respModel string,
	q Question,
	expectedKind QuestionType,
	ansType string,
	fields map[string]json.RawMessage,
) (Answer, error) {
	switch ansType {
	case "refusal":
		return Answer{
			Type:     AnswerRefusal,
			Name:     q.Name,
			Provider: provider,
			Model:    respModel,
		}, nil

	case "predicate":
		if expectedKind != QuestionPredicate {
			return Answer{}, protocolError(provider, displayName, "returned an unexpected answer type")
		}
		prob, err := parseRequiredProbability(fields, "probability")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		conf, err := parseOptionalProbability(fields, "confidence")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		return Answer{
			Type:        AnswerPredicate,
			Name:        q.Name,
			Probability: prob,
			Confidence:  conf,
			Provider:    provider,
			Model:       respModel,
		}, nil

	case "choice":
		if expectedKind != QuestionChoice {
			return Answer{}, protocolError(provider, displayName, "returned an unexpected answer type")
		}
		rawChoice, ok := fields["choice"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted selected choice")
		}
		var selected ChoiceValue
		if err := json.Unmarshal(rawChoice, &selected); err != nil {
			return Answer{}, protocolError(provider, displayName, "returned an invalid choice value")
		}
		conf, err := parseRequiredProbability(fields, "confidence")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		declared := make(map[ChoiceValue]bool, len(q.Choices))
		for _, opt := range q.Choices {
			declared[opt.Value] = true
		}
		if !declared[selected] {
			return Answer{}, protocolError(provider, displayName, "returned an undeclared choice value")
		}

		rawProbs, ok := fields["probabilities"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted choice probabilities")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(rawProbs, &items); err != nil || items == nil {
			return Answer{}, protocolError(provider, displayName, "returned invalid choice probabilities")
		}
		probByOpt := make(map[ChoiceValue]float64, len(q.Choices))
		sum := 0.0
		for _, rawItem := range items {
			itemFields, err := decodeUniqueObject(rawItem)
			if err != nil {
				return Answer{}, protocolError(provider, displayName, "returned an invalid choice probability entry")
			}
			rawVal, hasVal := itemFields["value"]
			if !hasVal {
				return Answer{}, protocolError(provider, displayName, "omitted choice probability value")
			}
			var optVal ChoiceValue
			if err := json.Unmarshal(rawVal, &optVal); err != nil || !declared[optVal] {
				return Answer{}, protocolError(provider, displayName, "returned an undeclared or invalid choice probability option")
			}
			if _, dup := probByOpt[optVal]; dup {
				return Answer{}, protocolError(provider, displayName, "returned duplicate choice probability options")
			}
			p, err := parseRequiredProbability(itemFields, "probability")
			if err != nil {
				return Answer{}, protocolError(provider, displayName, err.Error())
			}
			probByOpt[optVal] = *p
			sum += *p
		}
		if len(probByOpt) != len(q.Choices) {
			return Answer{}, protocolError(provider, displayName, "omitted declared choice probability options")
		}
		if !validProbabilitySum(sum) {
			return Answer{}, protocolError(provider, displayName, "returned choice probabilities that do not sum to 1")
		}
		orderedProbs := make([]ChoiceProbability, len(q.Choices))
		for i, opt := range q.Choices {
			orderedProbs[i] = ChoiceProbability{
				Value:       opt.Value,
				Probability: probByOpt[opt.Value],
			}
		}
		return Answer{
			Type:                AnswerChoice,
			Name:                q.Name,
			Choice:              &selected,
			ChoiceProbabilities: orderedProbs,
			Confidence:          conf,
			Provider:            provider,
			Model:               respModel,
		}, nil

	case "score":
		if expectedKind != QuestionScore {
			return Answer{}, protocolError(provider, displayName, "returned an unexpected answer type")
		}
		rawScore, ok := fields["score"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted score value")
		}
		scoreVal, err := parseJSONFloat(rawScore)
		if err != nil || !validScore(&scoreVal, len(q.Levels)) {
			return Answer{}, protocolError(provider, displayName, "returned an invalid score value")
		}
		scoreVal = clampScoreZero(scoreVal, len(q.Levels))
		conf, err := parseRequiredProbability(fields, "confidence")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		rawProbs, ok := fields["probabilities"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted score probabilities")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(rawProbs, &items); err != nil || items == nil {
			return Answer{}, protocolError(provider, displayName, "returned invalid score probabilities")
		}
		probByIndex := make(map[int]float64, len(q.Levels))
		sum := 0.0
		for _, rawItem := range items {
			itemFields, err := decodeUniqueObject(rawItem)
			if err != nil {
				return Answer{}, protocolError(provider, displayName, "returned an invalid score probability entry")
			}
			rawIdx, hasIdx := itemFields["value"]
			if !hasIdx {
				return Answer{}, protocolError(provider, displayName, "omitted score level index")
			}
			idx, err := parseJSONInt(rawIdx)
			if err != nil || idx < 0 || idx >= len(q.Levels) {
				return Answer{}, protocolError(provider, displayName, "returned an out-of-range score level index")
			}
			if _, dup := probByIndex[idx]; dup {
				return Answer{}, protocolError(provider, displayName, "returned duplicate score level index")
			}
			if rawLabel, hasLabel := itemFields["label"]; hasLabel && !bytes.Equal(bytes.TrimSpace(rawLabel), []byte("null")) {
				label, err := parseJSONString(rawLabel)
				if err != nil || (label != "" && label != q.Levels[idx].Label) {
					return Answer{}, protocolError(provider, displayName, "returned a mismatched score level label")
				}
			}
			p, err := parseRequiredProbability(itemFields, "probability")
			if err != nil {
				return Answer{}, protocolError(provider, displayName, err.Error())
			}
			probByIndex[idx] = *p
			sum += *p
		}
		if len(probByIndex) != len(q.Levels) {
			return Answer{}, protocolError(provider, displayName, "omitted declared score level probabilities")
		}
		if !validProbabilitySum(sum) {
			return Answer{}, protocolError(provider, displayName, "returned score probabilities that do not sum to 1")
		}
		orderedProbs := make([]ScoreProbability, len(q.Levels))
		for i, lvl := range q.Levels {
			orderedProbs[i] = ScoreProbability{
				Index:       i,
				Label:       lvl.Label,
				Probability: probByIndex[i],
			}
		}
		return Answer{
			Type:               AnswerScore,
			Name:               q.Name,
			Score:              &scoreVal,
			ScoreProbabilities: orderedProbs,
			Confidence:         conf,
			Provider:           provider,
			Model:              respModel,
		}, nil

	default:
		return Answer{}, protocolError(provider, displayName, "returned an unexpected answer type")
	}
}

func parseRequiredProbability(fields map[string]json.RawMessage, key string) (*float64, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	v, err := parseJSONFloat(raw)
	if err != nil || !validProbabilityValue(v) {
		return nil, fmt.Errorf("returned an invalid %s", key)
	}
	return &v, nil
}

func parseOptionalProbability(fields map[string]json.RawMessage, key string) (*float64, error) {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	v, err := parseJSONFloat(raw)
	if err != nil || !validProbabilityValue(v) {
		return nil, fmt.Errorf("returned an invalid %s", key)
	}
	return &v, nil
}

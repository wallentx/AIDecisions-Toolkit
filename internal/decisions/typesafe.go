package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	TypeSafeDefaultModel   = "jev-latest"
	TypeSafeEndpoint       = "https://api.typesafe.ai/v1/systemone"
	typeSafeDisplayName    = "TypeSafe System One API"
	typeSafeMaxScoreLevels = 10
)

type typeSafeAdapter struct{}

type systemOneWireRequest struct {
	Model     string                           `json:"model"`
	State     string                           `json:"state"`
	Questions map[string]systemOneWireQuestion `json:"questions"`
}

type systemOneWireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func (typeSafeAdapter) resolveTarget(model, endpoint string) (string, string, error) {
	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		resolvedModel = TypeSafeDefaultModel
	}
	resolvedEndpoint := strings.TrimSpace(endpoint)
	if resolvedEndpoint == "" {
		resolvedEndpoint = TypeSafeEndpoint
	}
	if _, err := ValidateEndpoint(resolvedEndpoint); err != nil {
		return "", "", err
	}
	return resolvedModel, resolvedEndpoint, nil
}

func (a typeSafeAdapter) preview(req Request, defaultModel, endpoint string) (Preview, error) {
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
	body, _, err := encodeSystemOneWireRequest(ProviderTypeSafe, model, req, 0, typeSafeMaxScoreLevels)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		Provider: ProviderTypeSafe,
		Endpoint: targetURL,
		Model:    model,
		Request:  body,
	}, nil
}

func (a typeSafeAdapter) decide(ctx context.Context, c Client, req Request) (Response, error) {
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
	body, choiceMaps, err := encodeSystemOneWireRequest(ProviderTypeSafe, model, req, 0, typeSafeMaxScoreLevels)
	if err != nil {
		return Response{}, err
	}
	if err := ValidateAPIKey(c.APIKey, ProviderTypeSafe); err != nil {
		return Response{}, err
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	respBytes, err := postJSON(ctx, ProviderTypeSafe, typeSafeDisplayName, targetURL, headers, body, c.HTTP, nil)
	if err != nil {
		return Response{}, err
	}
	return decodeSystemOneWireResponse(ProviderTypeSafe, typeSafeDisplayName, req, choiceMaps, false, false, respBytes)
}

func encodeSystemOneWireRequest(
	provider Provider,
	model string,
	req Request,
	maxQuestions int,
	scoreMaxLevels int,
) ([]byte, map[string]map[string]ChoiceValue, error) {
	if maxQuestions > 0 && len(req.Questions) > maxQuestions {
		return nil, nil, &UnsupportedError{
			Provider: provider,
			Feature:  fmt.Sprintf("more than %d questions per request (got %d)", maxQuestions, len(req.Questions)),
		}
	}
	questions := make(map[string]systemOneWireQuestion, len(req.Questions))
	choiceMaps := make(map[string]map[string]ChoiceValue)
	for _, q := range req.Questions {
		kind, err := canonicalQuestionType(q.Type)
		if err != nil {
			return nil, nil, err
		}
		wq := systemOneWireQuestion{Instructions: q.Instructions}
		switch kind {
		case QuestionPredicate:
			wq.Type = "noul"
			if q.Criteria != nil {
				crit := make(map[string]string, 2)
				if strings.TrimSpace(q.Criteria.TrueDescription) != "" {
					crit["true"] = q.Criteria.TrueDescription
				}
				if strings.TrimSpace(q.Criteria.FalseDescription) != "" {
					crit["false"] = q.Criteria.FalseDescription
				}
				if len(crit) > 0 {
					wq.Criteria = crit
				}
			}
		case QuestionChoice:
			wq.Type = "choice"
			criteria, byKey, err := buildStringKeyedChoices(provider, q)
			if err != nil {
				return nil, nil, err
			}
			wq.Criteria = criteria
			choiceMaps[q.Name] = byKey
		case QuestionScore:
			if scoreMaxLevels > 0 && len(q.Levels) > scoreMaxLevels {
				return nil, nil, &UnsupportedError{
					Provider: provider,
					Feature:  fmt.Sprintf("score questions with more than %d levels on question %q", scoreMaxLevels, q.Name),
				}
			}
			wq.Type = "score"
			levels := make([]string, len(q.Levels))
			for i, lvl := range q.Levels {
				levels[i] = formatScoreCriterion(lvl)
			}
			wq.Criteria = levels
		}
		questions[q.Name] = wq
	}
	wire := systemOneWireRequest{
		Model:     model,
		State:     req.Input,
		Questions: questions,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, err
	}
	return body, choiceMaps, nil
}

// decodeMapKeyedResponse normalizes both TypeSafe System One and Google Gemini
// answer maps, enforcing unique keys, declared option sets, and confidence rules.
func decodeSystemOneWireResponse(
	provider Provider,
	displayName string,
	req Request,
	choiceMaps map[string]map[string]ChoiceValue,
	requireNoulConfidence bool,
	allowRefusal bool,
	data []byte,
) (Response, error) {
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
	answerMap, err := decodeUniqueObject(rawAnswers)
	if err != nil {
		return Response{}, protocolError(provider, displayName, "returned invalid answers object")
	}

	questionsByName := make(map[string]Question, len(req.Questions))
	for _, q := range req.Questions {
		questionsByName[q.Name] = q
	}
	for name := range answerMap {
		if _, known := questionsByName[name]; !known {
			return Response{}, protocolError(provider, displayName, "returned an unexpected answer name")
		}
	}
	if len(answerMap) != len(req.Questions) {
		return Response{}, protocolError(provider, displayName, "omitted requested answers")
	}

	answers := make(map[string]Answer, len(req.Questions))
	ordered := make([]Answer, len(req.Questions))
	for i, q := range req.Questions {
		fields, err := decodeUniqueObject(answerMap[q.Name])
		if err != nil {
			return Response{}, protocolError(provider, displayName, "returned an invalid answer object")
		}
		rawType, hasType := fields["type"]
		ansType, typeErr := parseJSONString(rawType)
		if !hasType || typeErr != nil {
			return Response{}, protocolError(provider, displayName, "returned an answer without a valid type")
		}
		expectedKind, _ := canonicalQuestionType(q.Type)
		ans, err := normalizeMapKeyedAnswer(
			provider,
			displayName,
			respModel,
			q,
			expectedKind,
			ansType,
			choiceMaps[q.Name],
			requireNoulConfidence,
			allowRefusal,
			fields,
		)
		if err != nil {
			return Response{}, err
		}
		answers[q.Name] = ans
		ordered[i] = ans
	}
	return Response{
		Provider: provider,
		Model:    respModel,
		Answers:  answers,
		Ordered:  ordered,
	}, nil
}

func normalizeMapKeyedAnswer(
	provider Provider,
	displayName string,
	respModel string,
	q Question,
	expectedKind QuestionType,
	ansType string,
	byKey map[string]ChoiceValue,
	requireNoulConfidence bool,
	allowRefusal bool,
	fields map[string]json.RawMessage,
) (Answer, error) {
	if allowRefusal && ansType == "refusal" {
		return Answer{
			Type:     AnswerRefusal,
			Name:     q.Name,
			Provider: provider,
			Model:    respModel,
		}, nil
	}
	switch ansType {
	case "noul":
		if expectedKind != QuestionPredicate {
			return Answer{}, protocolError(provider, displayName, "returned an unexpected answer type")
		}
		prob, err := parseRequiredProbability(fields, "noul")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		var conf *float64
		if requireNoulConfidence {
			conf, err = parseRequiredProbability(fields, "confidence")
		} else {
			conf, err = parseOptionalProbability(fields, "confidence")
		}
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
		choiceKey, err := parseJSONString(rawChoice)
		if err != nil {
			return Answer{}, protocolError(provider, displayName, "returned an invalid choice key")
		}
		selected, exists := byKey[choiceKey]
		if !exists {
			return Answer{}, protocolError(provider, displayName, "returned an undeclared choice value")
		}
		conf, err := parseRequiredProbability(fields, "confidence")
		if err != nil {
			return Answer{}, protocolError(provider, displayName, err.Error())
		}
		rawProbs, ok := fields["probabilities"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted choice probabilities")
		}
		probFields, err := decodeUniqueObject(rawProbs)
		if err != nil {
			return Answer{}, protocolError(provider, displayName, "returned invalid choice probabilities")
		}
		if len(probFields) != len(q.Choices) {
			return Answer{}, protocolError(provider, displayName, "returned mismatched choice probability options")
		}
		probByOpt := make(map[ChoiceValue]float64, len(q.Choices))
		sum := 0.0
		for key, rawProb := range probFields {
			optVal, declared := byKey[key]
			if !declared {
				return Answer{}, protocolError(provider, displayName, "returned an undeclared choice probability option")
			}
			p, err := parseJSONFloat(rawProb)
			if err != nil || !validProbabilityValue(p) {
				return Answer{}, protocolError(provider, displayName, "returned an invalid choice probability")
			}
			probByOpt[optVal] = p
			sum += p
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
		if rawLegend, hasLegend := fields["legend"]; hasLegend && !bytes.Equal(bytes.TrimSpace(rawLegend), []byte("null")) {
			legendFields, err := decodeUniqueObject(rawLegend)
			if err != nil || len(legendFields) != len(q.Levels) {
				return Answer{}, protocolError(provider, displayName, "returned an invalid score legend")
			}
			for i := range q.Levels {
				rawEntry, exists := legendFields[strconv.Itoa(i)]
				if !exists {
					return Answer{}, protocolError(provider, displayName, "returned an invalid score legend entry")
				}
				if _, err := parseJSONString(rawEntry); err != nil {
					return Answer{}, protocolError(provider, displayName, "returned an invalid score legend entry")
				}
			}
		}
		rawProbs, ok := fields["probabilities"]
		if !ok {
			return Answer{}, protocolError(provider, displayName, "omitted score probabilities")
		}
		probFields, err := decodeUniqueObject(rawProbs)
		if err != nil || len(probFields) != len(q.Levels) {
			return Answer{}, protocolError(provider, displayName, "returned invalid score probabilities")
		}
		orderedProbs := make([]ScoreProbability, len(q.Levels))
		sum := 0.0
		for i, lvl := range q.Levels {
			rawProb, exists := probFields[strconv.Itoa(i)]
			if !exists {
				return Answer{}, protocolError(provider, displayName, "omitted a declared score level probability")
			}
			p, err := parseJSONFloat(rawProb)
			if err != nil || !validProbabilityValue(p) {
				return Answer{}, protocolError(provider, displayName, "returned an invalid score probability")
			}
			orderedProbs[i] = ScoreProbability{
				Index:       i,
				Label:       lvl.Label,
				Probability: p,
			}
			sum += p
		}
		if !validProbabilitySum(sum) {
			return Answer{}, protocolError(provider, displayName, "returned score probabilities that do not sum to 1")
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

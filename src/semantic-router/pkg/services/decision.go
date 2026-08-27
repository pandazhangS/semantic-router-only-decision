package services

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/decision"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/selection"
)

// ErrClassifierUnavailable is returned when no classifier is available for
// the requested routing model.
var ErrClassifierUnavailable = errors.New("classifier is unavailable")

// DecideRequest is the decision-service input. It reuses the intent
// classification request contract (text, messages, metadata, tools).
type DecideRequest struct {
	IntentRequest
}

// ModelCandidate is one candidate model from the matched decision.
type ModelCandidate struct {
	Model           string `json:"model"`
	LoRAName        string `json:"lora_name,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	UseReasoning    bool   `json:"use_reasoning,omitempty"`
}

// DecideResponse is the decision-service output: the selected model id plus
// the full candidate model list from the matched decision.
type DecideResponse struct {
	ModelID          string            `json:"model_id"`
	ModelList        []ModelCandidate  `json:"model_list,omitempty"`
	DecisionName     string            `json:"decision_name,omitempty"`
	Confidence       float64           `json:"confidence,omitempty"`
	MatchedRules     []string          `json:"matched_rules,omitempty"`
	Category         string            `json:"category,omitempty"`
	Recipe           config.RecipeName `json:"recipe,omitempty"`
	SelectionStatus  string            `json:"selection_status,omitempty"`
	SelectionMethod  string            `json:"selection_method,omitempty"`
	SelectionReason  string            `json:"selection_reason,omitempty"`
	ProcessingTimeMs int64             `json:"processing_time_ms"`
}

// DecideModel evaluates the request signals, matches a routing decision, and
// returns the selected model id together with the decision's candidate list.
// The selected model is produced by the same runtime selector registry used
// by the data plane (via EvalModelSelector), so algorithm-aware decisions
// (elo, hybrid, multi_factor, ...) resolve identically to ExtProc routing.
func (s *ClassificationService) DecideModel(req DecideRequest) (*DecideResponse, error) {
	start := time.Now()

	input, err := req.IntentRequest.resolveSignalInput()
	if err != nil {
		return nil, err
	}
	classifier, runtimeConfig, err := s.runtimeSnapshotForRequestModel(req.Model)
	if err != nil {
		return nil, err
	}
	if classifier == nil {
		return nil, ErrClassifierUnavailable
	}

	forceEvaluateAll := req.Options != nil && req.Options.EvaluateAllSignals
	signals := classifier.EvaluateAllSignalsWithRequestFacts(
		input.evaluationText,
		input.contextText,
		input.currentUserText,
		input.priorUserMessages,
		input.nonUserMessages,
		input.hasAssistantReply,
		forceEvaluateAll,
		"",
		nil,
		input.conversationFacts,
		input.imageURL,
		input.requestFacts,
	)

	var decisionResult *decision.DecisionResult
	if classifier.Config != nil && len(classifier.Config.Decisions) > 0 {
		decisionResult, err = classifier.EvaluateDecisionWithEngine(signals)
		if err != nil && !strings.Contains(err.Error(), "no decisions configured") {
			return nil, err
		}
	}

	category, confidence := resolveIntentCategory(classifier, decisionResult, input.evaluationText)

	resp := &DecideResponse{
		Category:         category,
		Confidence:       confidence,
		ProcessingTimeMs: time.Since(start).Milliseconds(),
	}

	if decisionResult != nil && decisionResult.Decision != nil {
		matched := decisionResult.Decision
		resp.DecisionName = matched.Name
		resp.Confidence = decisionResult.Confidence
		resp.MatchedRules = decisionResult.MatchedRules
		resp.ModelList = modelCandidatesFromRefs(matched.ModelRefs)
		if recipe, ok := s.recipeForRequestModel(req.Model); ok {
			resp.Recipe = recipe.Name
		}
		modelID, method, reason := selectModelFromDecision(
			matched,
			input.evaluationText,
			category,
			resp.Recipe,
			input.requestFacts.ContextTokenFloor,
		)
		resp.ModelID = modelID
		resp.SelectionMethod = method
		resp.SelectionReason = reason
		if modelID != "" {
			resp.SelectionStatus = "selected"
		}
	} else if runtimeConfig != nil {
		resp.ModelID = runtimeConfig.DefaultModel
		resp.DecisionName = "default"
	}

	return resp, nil
}

// selectModelFromDecision picks the model id for a matched decision using the
// same selector registry as the data plane. Single-candidate decisions and
// static algorithms resolve to the first declared model; algorithm-aware
// decisions (elo, hybrid, multi_factor, ...) go through the registered
// selector, falling back to the first candidate when the selector is
// unavailable or fails.
func selectModelFromDecision(
	matched *config.Decision,
	query string,
	category string,
	recipe config.RecipeName,
	contextTokenCount int,
) (string, string, string) {
	if matched == nil || len(matched.ModelRefs) == 0 {
		return "", "", ""
	}
	if len(matched.ModelRefs) == 1 {
		ref := matched.ModelRefs[0]
		return ref.Model, "static", "single declared candidate"
	}

	method := selectionMethodForAlgorithm(matched.Algorithm)
	if method == selection.MethodStatic {
		ref := matched.ModelRefs[0]
		return ref.Model, "static", "first declared candidate"
	}

	selCtx := &selection.SelectionContext{
		Query:               query,
		DecisionName:        matched.Name,
		RecipeName:          recipe,
		CategoryName:        category,
		CandidateModels:     matched.ModelRefs,
		CandidateIterations: matched.CandidateIterations,
	}
	result, err := selection.Select(context.Background(), method, selCtx)
	if err != nil || result == nil || result.SelectedModel == "" {
		ref := matched.ModelRefs[0]
		return ref.Model, string(method), "selector failed, fallback to first candidate"
	}
	reason := strings.TrimSpace(result.Reasoning)
	if reason == "" {
		reason = "selected by the runtime selector"
	}
	return result.SelectedModel, string(method), reason
}

func selectionMethodForAlgorithm(algorithm *config.AlgorithmConfig) selection.SelectionMethod {
	if algorithm == nil || strings.TrimSpace(algorithm.Type) == "" {
		return selection.MethodStatic
	}
	return selection.SelectionMethod(algorithm.Type)
}

func (s *ClassificationService) recipeForRequestModel(modelName string) (*config.RoutingRecipe, bool) {
	if s == nil || s.config == nil {
		return nil, false
	}
	s.configMutex.RLock()
	defer s.configMutex.RUnlock()
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		trimmed = config.DefaultVSRAutoModelName
	}
	return s.config.RecipeForRoutingModel(trimmed)
}

func modelCandidatesFromRefs(refs []config.ModelRef) []ModelCandidate {
	if len(refs) == 0 {
		return nil
	}
	candidates := make([]ModelCandidate, 0, len(refs))
	for _, ref := range refs {
		candidate := ModelCandidate{
			Model:           ref.Model,
			LoRAName:        ref.LoRAName,
			ReasoningEffort: ref.ReasoningEffort,
		}
		if ref.UseReasoning != nil {
			candidate.UseReasoning = *ref.UseReasoning
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

package classification

import (
	"context"
	"fmt"
	"time"

	candle_binding "github.com/vllm-project/semantic-router/candle-binding"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/observability/logging"
)

type CategoryInitializer interface {
	Init(modelID string, useCPU bool, numClasses ...int) error
}

type CategoryInitializerImpl struct {
	usedModernBERT bool // Track which init path succeeded for inference routing
}

func (c *CategoryInitializerImpl) Init(modelID string, useCPU bool, numClasses ...int) error {
	// A ModernBERT config.json is incompatible with the traditional Candle BERT
	// loader, so probing Candle first logs alarming "Failed to initialize" errors
	// before the ModernBERT fallback succeeds (#2096). When the model is detected
	// as ModernBERT, try the ModernBERT initializer first to skip that doomed probe.
	if isModernBertModel(modelID) {
		if err := candle_binding.InitModernBertClassifier(modelID, useCPU); err == nil {
			c.usedModernBERT = true
			logging.ComponentEvent("classifier", "category_classifier_initialized", map[string]interface{}{
				"backend":   "modernbert",
				"model_ref": modelID,
			})
			return nil
		}
		// Detected as ModernBERT but its initializer failed (e.g. a LoRA model
		// whose base is ModernBERT); fall through to the auto-detect path.
		logging.ComponentDebugEvent("classifier", "category_classifier_fallback_enabled", map[string]interface{}{
			"fallback_backend": "candle_bert_auto",
			"model_ref":        modelID,
		})
	}

	// Try auto-detecting Candle BERT init - checks for lora_config.json
	// This enables LoRA Intent/Category models when available
	success := candle_binding.InitCandleBertClassifier(modelID, numClasses[0], useCPU)
	if success {
		c.usedModernBERT = false
		logging.ComponentEvent("classifier", "category_classifier_initialized", map[string]interface{}{
			"backend":   "candle_bert_auto",
			"model_ref": modelID,
		})
		return nil
	}

	// Fallback to ModernBERT-specific init for backward compatibility
	logging.ComponentDebugEvent("classifier", "category_classifier_fallback_enabled", map[string]interface{}{
		"fallback_backend": "modernbert",
		"model_ref":        modelID,
	})
	err := candle_binding.InitModernBertClassifier(modelID, useCPU)
	if err != nil {
		return fmt.Errorf("failed to initialize category classifier (both auto-detect and ModernBERT): %w", err)
	}
	c.usedModernBERT = true
	logging.ComponentEvent("classifier", "category_classifier_initialized", map[string]interface{}{
		"backend":   "modernbert",
		"model_ref": modelID,
	})
	return nil
}

// MmBERT32KCategoryInitializerImpl uses mmBERT-32K (YaRN RoPE, 32K context) for intent classification.
type MmBERT32KCategoryInitializerImpl struct {
	usedMmBERT32K bool
}

func (c *MmBERT32KCategoryInitializerImpl) Init(modelID string, useCPU bool, numClasses ...int) error {
	backend := embeddingBackendOverride()
	if backend == "openvino" {
		nc := 0
		if len(numClasses) > 0 {
			nc = numClasses[0]
		}
		if ovErr := initOpenVINOClassifier(modelID, nc, useCPU); ovErr == nil {
			c.usedMmBERT32K = true
			logging.ComponentEvent("classifier", "category_classifier_initialized", map[string]interface{}{
				"backend":   "openvino",
				"model_ref": modelID,
				"classes":   nc,
			})
			return nil
		} else {
			logging.Warnf("OpenVINO classifier init failed, falling back to candle: %v", ovErr)
		}
	}

	err := candle_binding.InitMmBert32KIntentClassifier(modelID, useCPU)
	if err != nil {
		return fmt.Errorf("failed to initialize mmBERT-32K intent classifier: %w", err)
	}
	c.usedMmBERT32K = true
	logging.ComponentEvent("classifier", "category_classifier_initialized", map[string]interface{}{
		"backend":   "mmbert_32k",
		"model_ref": modelID,
	})
	return nil
}

// createCategoryInitializer creates the category initializer (auto-detecting).
func createCategoryInitializer() CategoryInitializer {
	return &CategoryInitializerImpl{}
}

// createMmBERT32KCategoryInitializer creates an mmBERT-32K category initializer.
func createMmBERT32KCategoryInitializer() CategoryInitializer {
	return &MmBERT32KCategoryInitializerImpl{}
}

type CategoryInference interface {
	Classify(text string) (candle_binding.ClassResult, error)
	ClassifyWithProbabilities(text string) (candle_binding.ClassResultWithProbs, error)
}

type CategoryInferenceImpl struct{}

func (c *CategoryInferenceImpl) Classify(text string) (candle_binding.ClassResult, error) {
	// Try Candle BERT first, fall back to ModernBERT if it fails
	result, err := candle_binding.ClassifyCandleBertText(text)
	if err != nil {
		// Candle BERT not initialized or failed, try ModernBERT
		return candle_binding.ClassifyModernBertText(text)
	}
	return result, nil
}

func (c *CategoryInferenceImpl) ClassifyWithProbabilities(text string) (candle_binding.ClassResultWithProbs, error) {
	// Note: CandleBert doesn't have WithProbabilities yet, fall back to ModernBERT
	// This will work correctly if ModernBERT was initialized as fallback
	return candle_binding.ClassifyModernBertTextWithProbabilities(text)
}

// createCategoryInference creates the category inference (auto-detecting).
func createCategoryInference() CategoryInference {
	return &CategoryInferenceImpl{}
}

// MmBERT32KCategoryInferenceImpl uses mmBERT-32K for intent classification.
// It supports both candle and OpenVINO backends via EMBEDDING_BACKEND_OVERRIDE.
type MmBERT32KCategoryInferenceImpl struct{}

func (c *MmBERT32KCategoryInferenceImpl) getBackend() string {
	if backend := embeddingBackendOverride(); backend != "" {
		return backend
	}
	return "candle"
}

func (c *MmBERT32KCategoryInferenceImpl) Classify(text string) (candle_binding.ClassResult, error) {
	backend := c.getBackend()
	start := time.Now()
	var result candle_binding.ClassResult
	var err error

	switch backend {
	case "openvino":
		ovResult, ovErr := classifyOpenVINO(text)
		if ovErr != nil {
			err = ovErr
		} else {
			result = candle_binding.ClassResult{
				Class:      ovResult.Class,
				Confidence: ovResult.Confidence,
			}
		}
	default:
		result, err = candle_binding.ClassifyMmBert32KIntent(text)
	}

	elapsed := time.Since(start)
	logging.Infof("[Perf] classifier inference (phase=request, backend=%s): %.3fms",
		backend, float64(elapsed.Microseconds())/1000.0)

	return result, err
}

func (c *MmBERT32KCategoryInferenceImpl) ClassifyWithProbabilities(text string) (candle_binding.ClassResultWithProbs, error) {
	result, err := c.Classify(text)
	if err != nil {
		return candle_binding.ClassResultWithProbs{}, err
	}
	return candle_binding.ClassResultWithProbs{
		Class:      result.Class,
		Confidence: result.Confidence,
	}, nil
}

// createMmBERT32KCategoryInference creates mmBERT-32K category inference.
func createMmBERT32KCategoryInference() CategoryInference {
	return &MmBERT32KCategoryInferenceImpl{}
}

// createRemoteCategoryInference serves the domain classifier from an external
// http_classify service (category_model.protocol), mirroring the jailbreak
// remote-backend selection. The response label/score distribution is aligned
// against the configured CategoryMapping by name, so the external service
// must serve every label of that mapping.
func createRemoteCategoryInference(cfg *config.RouterConfig, mapping *CategoryMapping) (CategoryInference, error) {
	if cfg.CategoryModel.Protocol != config.CategoryProtocolHTTPClassify {
		return nil, fmt.Errorf(
			"category_model.protocol: unrecognized value %q (supported: %s)",
			cfg.CategoryModel.Protocol,
			config.CategoryProtocolHTTPClassify,
		)
	}
	if isNilMapping(mapping) {
		return nil, fmt.Errorf(
			"category_mapping_path is required for category_model.protocol=%s",
			config.CategoryProtocolHTTPClassify,
		)
	}
	externalCfg := cfg.FindExternalModelByRole(config.ModelRoleClassification)
	if externalCfg == nil {
		return nil, fmt.Errorf(
			"external model with model_role='%s' is required for category_model.protocol=%s",
			config.ModelRoleClassification,
			config.CategoryProtocolHTTPClassify,
		)
	}
	if externalCfg.ModelEndpoint.Address == "" {
		return nil, fmt.Errorf("external classification model endpoint address is required")
	}
	backend, err := NewHTTPClassifierInference(externalCfg, mapping)
	if err != nil {
		return nil, err
	}
	return &remoteCategoryInference{backend: backend}, nil
}

// remoteCategoryInference adapts the http_classify SequenceClassifierBackend
// to the CategoryInference contract. The argmax/class confidence is derived
// once here (via deriveArgmax) so every category backend - local candle,
// mmBERT-32K, or remote HTTP - feeds matchDomainCategories identically.
type remoteCategoryInference struct {
	backend SequenceClassifierBackend
}

func (r *remoteCategoryInference) Classify(text string) (candle_binding.ClassResult, error) {
	result, err := r.backend.Classify(context.Background(), text)
	if err != nil {
		return candle_binding.ClassResult{}, err
	}
	class, confidence := deriveArgmax(result.Probabilities)
	return candle_binding.ClassResult{Class: class, Confidence: confidence}, nil
}

func (r *remoteCategoryInference) ClassifyWithProbabilities(text string) (candle_binding.ClassResultWithProbs, error) {
	result, err := r.backend.Classify(context.Background(), text)
	if err != nil {
		return candle_binding.ClassResultWithProbs{}, err
	}
	class, confidence := deriveArgmax(result.Probabilities)
	return candle_binding.ClassResultWithProbs{
		Class:         class,
		Confidence:    confidence,
		Probabilities: result.Probabilities,
		NumClasses:    len(result.Probabilities),
	}, nil
}

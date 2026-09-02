package classification

import (
	"fmt"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/observability/logging"
)

func (b *classifierOptionBuilder) addCategoryClassifier(categoryMapping *CategoryMapping) error {
	categoryModel := b.cfg.CategoryModel
	if categoryModel.ModelID == "" && categoryModel.Protocol == "" {
		return nil
	}
	if categoryModel.Protocol != "" {
		categoryInference, err := createRemoteCategoryInference(b.cfg, categoryMapping)
		if err != nil {
			return err
		}
		logging.ComponentEvent("classifier", "category_classifier_backend_selected", map[string]interface{}{
			"backend":  "http_classify",
			"protocol": categoryModel.Protocol,
			"model":    categoryModel.ModelID,
		})
		// Remote backends have no local model to initialize.
		b.options = append(b.options, withCategory(categoryMapping, nil, categoryInference))
		return nil
	}
	var categoryInitializer CategoryInitializer
	var categoryInference CategoryInference
	if categoryModel.UseMmBERT32K {
		logging.ComponentEvent("classifier", "category_classifier_backend_selected", map[string]interface{}{
			"backend": "mmbert_32k",
		})
		categoryInitializer = createMmBERT32KCategoryInitializer()
		categoryInference = createMmBERT32KCategoryInference()
	} else {
		categoryInitializer = createCategoryInitializer()
		categoryInference = createCategoryInference()
	}
	b.options = append(b.options, withCategory(categoryMapping, categoryInitializer, categoryInference))
	return nil
}

func buildJailbreakDependencies(cfg *config.RouterConfig, jailbreakMapping *JailbreakMapping) (JailbreakInitializer, SequenceClassifierBackend, error) {
	jailbreakInference, err := createJailbreakInference(&cfg.PromptGuard, cfg, jailbreakMapping)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create jailbreak inference: %w", err)
	}
	if cfg.PromptGuard.Protocol != "" {
		// Remote backends have no local model to initialize.
		return nil, jailbreakInference, nil
	}
	switch cfg.PromptGuard.Variant {
	case config.PromptGuardVariantMmBERT32K:
		return createMmBERT32KJailbreakInitializer(), jailbreakInference, nil
	default:
		return createJailbreakInitializer(), jailbreakInference, nil
	}
}

func buildPIIDependencies(cfg *config.RouterConfig) (PIIInitializer, PIIInference) {
	if cfg.PIIModel.UseMmBERT32K {
		logging.ComponentEvent("classifier", "pii_detector_backend_selected", map[string]interface{}{
			"backend": "mmbert_32k",
		})
		return createMmBERT32KPIIInitializer(), createMmBERT32KPIIInference()
	}
	return createPIIInitializer(), createPIIInference()
}

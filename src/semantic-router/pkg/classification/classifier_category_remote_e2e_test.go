package classification

import (
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
)

// TestDomainExternalService_EndToEnd verifies the optional external-domain
// scheme against a live domain-classifier service (real model, real candle
// FFI, real HTTP hop): the router-side construction path wires
// category_model.protocol=http_classify, and the domain signal evaluates
// through the service with sane, deterministic output.
//
// Guarded by env so normal `go test` runs skip it:
//   - DOMAIN_CLASSIFIER_E2E_URL:      e.g. http://127.0.0.1:18090
//   - DOMAIN_CLASSIFIER_E2E_MAPPING:  path to the real category_mapping.json
func TestDomainExternalService_EndToEnd(t *testing.T) {
	serviceURL := os.Getenv("DOMAIN_CLASSIFIER_E2E_URL")
	mappingPath := os.Getenv("DOMAIN_CLASSIFIER_E2E_MAPPING")
	if serviceURL == "" || mappingPath == "" {
		t.Skip("DOMAIN_CLASSIFIER_E2E_URL / DOMAIN_CLASSIFIER_E2E_MAPPING not set; E2E runs only against a live domain-classifier service")
	}

	mapping, err := LoadCategoryMapping(mappingPath)
	if err != nil {
		t.Fatalf("failed to load real category mapping: %v", err)
	}
	if mapping.LabelCount() < 2 {
		t.Fatalf("real category mapping has %d labels, want >= 2", mapping.LabelCount())
	}

	parsed, err := url.Parse(serviceURL)
	if err != nil {
		t.Fatalf("failed to parse service URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("failed to parse service port: %v", err)
	}

	cfg := &config.RouterConfig{
		InlineModels: config.InlineModels{
			Classifier: config.Classifier{
				CategoryModel: config.CategoryModel{
					ModelID:             "e2e-domain-classifier",
					Protocol:            config.CategoryProtocolHTTPClassify,
					Threshold:           0.2,
					CategoryMappingPath: mappingPath,
				},
			},
		},
		ExternalModels: []config.ExternalModelConfig{{
			Name:      "domain-classifier-svc",
			ModelRole: config.ModelRoleClassification,
			ModelEndpoint: config.ClassifierVLLMEndpoint{
				Address:  parsed.Hostname(),
				Port:     port,
				Protocol: parsed.Scheme,
			},
			ModelName: "domain-classifier",
		}},
	}

	// Real construction path: the same option-builder the router uses at
	// startup must wire the remote inference without any local model init.
	builder := newClassifierOptionBuilder(cfg, nil)
	if err := builder.addCategoryClassifier(mapping); err != nil {
		t.Fatalf("addCategoryClassifier failed: %v", err)
	}
	classifier := &Classifier{Config: cfg}
	for _, opt := range builder.options {
		opt(classifier)
	}
	if classifier.categoryInference == nil || classifier.categoryInitializer != nil {
		t.Fatalf("remote wiring incomplete: inference=%v initializer=%v",
			classifier.categoryInference != nil, classifier.categoryInitializer != nil)
	}

	evaluate := func(query string) (int, float64) {
		results := newSignalResultsForTest()
		var mu sync.Mutex
		classifier.evaluateDomainSignal(results, &mu, query)
		if errText, ok := results.SignalErrors["domain"]; ok && errText != "" {
			t.Fatalf("domain signal error: %s", errText)
		}
		if len(results.MatchedDomainRules) == 0 {
			t.Fatalf("no domain rules matched for %q (confidences: %v)", query, results.SignalConfidences)
		}
		for name, confidence := range results.SignalConfidences {
			if len(name) > 7 && name[:7] == "domain:" && (confidence <= 0 || confidence > 1) {
				t.Fatalf("confidence for %s out of (0,1]: %v", name, confidence)
			}
		}
		return len(results.MatchedDomainRules), results.Metrics.Domain.Confidence
	}

	firstRules, firstConfidence := evaluate("What is the derivative of x squared with respect to x?")
	secondRules, secondConfidence := evaluate("What is the derivative of x squared with respect to x?")
	if firstRules != secondRules || firstConfidence != secondConfidence {
		t.Fatalf("domain evaluation not deterministic: (%d, %v) vs (%d, %v)",
			firstRules, firstConfidence, secondRules, secondConfidence)
	}
	t.Logf("E2E OK: matchedRules=%d confidence=%.4f mapping=%s",
		firstRules, firstConfidence, mappingPath)
}

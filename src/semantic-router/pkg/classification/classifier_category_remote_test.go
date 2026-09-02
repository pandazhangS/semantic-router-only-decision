package classification

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
)

func remoteCategoryTestConfig(t *testing.T, serverURL string) (*config.RouterConfig, *CategoryMapping) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("failed to parse test server port: %v", err)
	}
	host := parsed.Hostname()
	mapping := &CategoryMapping{
		CategoryToIdx: map[string]int{"math": 0, "physics": 1},
		IdxToCategory: map[string]string{"0": "math", "1": "physics"},
	}
	cfg := &config.RouterConfig{
		InlineModels: config.InlineModels{
			Classifier: config.Classifier{
				CategoryModel: config.CategoryModel{
					ModelID:             "domain-classifier",
					Protocol:            config.CategoryProtocolHTTPClassify,
					CategoryMappingPath: "category_labels.json",
				},
			},
		},
		ExternalModels: []config.ExternalModelConfig{{
			Name:      "domain-classifier-svc",
			ModelRole: config.ModelRoleClassification,
			ModelEndpoint: config.ClassifierVLLMEndpoint{
				Address:  host,
				Port:     port,
				Protocol: "http",
			},
			ModelName: "domain-classifier",
		}},
	}
	return cfg, mapping
}

func TestCreateRemoteCategoryInference_ClassifyAlignsLabelsAndDerivesArgmax(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/classify" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]httpClassifyLabelScore{
			{Label: "physics", Score: 0.7},
			{Label: "math", Score: 0.3},
		})
	}))
	defer server.Close()

	cfg, mapping := remoteCategoryTestConfig(t, server.URL)
	inference, err := createRemoteCategoryInference(cfg, mapping)
	if err != nil {
		t.Fatalf("createRemoteCategoryInference failed: %v", err)
	}

	result, err := inference.ClassifyWithProbabilities("why does the ball fall")
	if err != nil {
		t.Fatalf("ClassifyWithProbabilities failed: %v", err)
	}
	if result.Class != 1 || result.Confidence != 0.7 {
		t.Fatalf("ClassWithProbs = %+v, want physics (class 1) with confidence 0.7", result)
	}
	if len(result.Probabilities) != 2 || result.Probabilities[1] != 0.7 || result.Probabilities[0] != 0.3 {
		t.Fatalf("Probabilities = %v, want [0.3 0.7]", result.Probabilities)
	}

	basic, err := inference.Classify("why does the ball fall")
	if err != nil {
		t.Fatalf("Classify failed: %v", err)
	}
	if basic.Class != 1 || basic.Confidence != 0.7 {
		t.Fatalf("Classify = %+v, want physics (class 1) with confidence 0.7", basic)
	}
}

func TestCreateRemoteCategoryInference_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not loaded", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg, mapping := remoteCategoryTestConfig(t, server.URL)
	inference, err := createRemoteCategoryInference(cfg, mapping)
	if err != nil {
		t.Fatalf("createRemoteCategoryInference failed: %v", err)
	}
	if _, err := inference.ClassifyWithProbabilities("anything"); err == nil {
		t.Fatalf("expected error from failing server, got nil")
	}
}

func TestCreateRemoteCategoryInference_ConfigErrors(t *testing.T) {
	mapping := &CategoryMapping{
		CategoryToIdx: map[string]int{"math": 0},
		IdxToCategory: map[string]string{"0": "math"},
	}

	cases := []struct {
		name    string
		cfg     *config.RouterConfig
		mapping *CategoryMapping
		wantSub string
	}{
		{
			name: "unrecognized protocol",
			cfg: &config.RouterConfig{
				InlineModels: config.InlineModels{
					Classifier: config.Classifier{
						CategoryModel: config.CategoryModel{ModelID: "x", Protocol: "grpc"},
					},
				},
			},
			mapping: mapping,
			wantSub: "unrecognized value",
		},
		{
			name: "missing mapping",
			cfg: &config.RouterConfig{
				InlineModels: config.InlineModels{
					Classifier: config.Classifier{
						CategoryModel: config.CategoryModel{ModelID: "x", Protocol: config.CategoryProtocolHTTPClassify},
					},
				},
				ExternalModels: []config.ExternalModelConfig{{
					Name:          "svc",
					ModelRole:     config.ModelRoleClassification,
					ModelEndpoint: config.ClassifierVLLMEndpoint{Address: "localhost", Port: 8090},
				}},
			},
			mapping: nil,
			wantSub: "category_mapping_path is required",
		},
		{
			name: "missing external model",
			cfg: &config.RouterConfig{
				InlineModels: config.InlineModels{
					Classifier: config.Classifier{
						CategoryModel: config.CategoryModel{ModelID: "x", Protocol: config.CategoryProtocolHTTPClassify},
					},
				},
			},
			mapping: mapping,
			wantSub: "external model with model_role",
		},
		{
			name: "missing endpoint address",
			cfg: &config.RouterConfig{
				InlineModels: config.InlineModels{
					Classifier: config.Classifier{
						CategoryModel: config.CategoryModel{ModelID: "x", Protocol: config.CategoryProtocolHTTPClassify},
					},
				},
				ExternalModels: []config.ExternalModelConfig{{
					Name:      "svc",
					ModelRole: config.ModelRoleClassification,
				}},
			},
			mapping: mapping,
			wantSub: "endpoint address is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createRemoteCategoryInference(tc.cfg, tc.mapping)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestAddCategoryClassifier_RemoteWiring proves the option-builder path: a
// remote category model produces a classifier with a remote inference and a
// nil local initializer (no candle model is loaded).
func TestAddCategoryClassifier_RemoteWiring(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]httpClassifyLabelScore{
			{Label: "math", Score: 0.6},
			{Label: "physics", Score: 0.4},
		})
	}))
	defer server.Close()

	cfg, mapping := remoteCategoryTestConfig(t, server.URL)
	builder := newClassifierOptionBuilder(cfg, nil)
	if err := builder.addCategoryClassifier(mapping); err != nil {
		t.Fatalf("addCategoryClassifier failed: %v", err)
	}

	classifier := &Classifier{}
	for _, opt := range builder.options {
		opt(classifier)
	}
	if classifier.categoryInference == nil {
		t.Fatalf("expected remote category inference to be wired")
	}
	if classifier.categoryInitializer != nil {
		t.Fatalf("remote mode must not create a local category initializer")
	}
	if classifier.CategoryMapping != mapping {
		t.Fatalf("category mapping not attached to classifier")
	}
}

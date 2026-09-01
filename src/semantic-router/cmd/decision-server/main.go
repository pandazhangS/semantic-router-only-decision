// Command decision-server runs the semantic routing decision core as a
// standalone HTTP service: it accepts a request context (OpenAI-style
// messages or plain text) and returns the selected model id plus the
// candidate model list. It does not start the Envoy ExtProc server, the
// Kubernetes controller, or any upstream forwarding path.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	npprof "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/classification"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/embedding"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/selection"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

// apiGuide is the operations guide (routing API, external embedding setup,
// threshold tuning) bundled into the binary and served at GET /docs.
//
//go:embed api_guide.md
var apiGuide string

// router holds the decision-core components. It is the trimmed equivalent of
// the full router's component graph: classifier (signal evaluation + decision
// engine) and the model-selector registry. No cache, replay, memory, tools,
// rate limiting, or upstream forwarding is constructed.
type router struct {
	cfg               *config.RouterConfig
	classificationSvc *services.ClassificationService
	modelSelector     *selection.Registry
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to the router config file")
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	enablePprof := flag.Bool("pprof", false, "expose /debug/pprof endpoints for CPU/heap profiling")
	flag.Parse()

	r, err := buildRouter(*configPath)
	if err != nil {
		log.Fatalf("failed to build router: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/decide", handleDecide(r.classificationSvc))
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /ready", handleReady)
	mux.HandleFunc("GET /docs", handleDocs)
	if *enablePprof {
		mux.HandleFunc("GET /debug/pprof/", npprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", npprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", npprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", npprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", npprof.Trace)
		log.Printf("pprof endpoints enabled at /debug/pprof/")
	}

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("decision server listening on %s", *listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

// buildRouter loads the config and constructs the decision-core components:
// recipe classifiers (signals + decision engine) and the model-selector
// registry. Classifier mappings (category/PII/jailbreak) are optional and
// loaded only when the config requires them.
func buildRouter(configPath string) (*router, error) {
	cfg, err := config.Parse(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	mappings, err := loadClassifierMappings(cfg)
	if err != nil {
		return nil, err
	}

	classifiers, err := classification.BuildRecipeClassifiers(
		cfg,
		mappings.categoryMapping,
		mappings.piiMapping,
		mappings.jailbreakMapping,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build recipe classifiers: %w", err)
	}
	if err := classifiers.InitializeRuntime(); err != nil {
		return nil, fmt.Errorf("failed to initialize recipe classifiers: %w", err)
	}
	if classifiers.Default() == nil {
		return nil, fmt.Errorf("default routing recipe classifier is unavailable")
	}

	classificationSvc := services.NewRecipeClassificationService(classifiers, cfg)

	registry := buildSelectionRegistry(cfg)
	selection.GlobalRegistry = registry

	return &router{
		cfg:               cfg,
		classificationSvc: classificationSvc,
		modelSelector:     registry,
	}, nil
}

type classifierMappings struct {
	categoryMapping  *classification.CategoryMapping
	piiMapping       *classification.PIIMapping
	jailbreakMapping *classification.JailbreakMapping
}

func loadClassifierMappings(cfg *config.RouterConfig) (*classifierMappings, error) {
	mappings := &classifierMappings{}
	var err error

	if cfg.NeedsCategoryMappingForRouting() {
		mappings.categoryMapping, err = classification.LoadCategoryMapping(cfg.CategoryMappingPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load category mapping: %w", err)
		}
	}
	if cfg.NeedsPIIMappingForRouting() {
		mappings.piiMapping, err = classification.LoadPIIMapping(cfg.PIIMappingPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load PII mapping: %w", err)
		}
	}
	if cfg.NeedsJailbreakMappingForRouting() {
		mappings.jailbreakMapping, err = classification.LoadJailbreakMapping(cfg.PromptGuard.JailbreakMappingPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load jailbreak mapping: %w", err)
		}
	}
	return mappings, nil
}

// buildSelectionRegistry constructs the model-selector registry from the
// global model_selection configuration. ML-based selectors (KNN/KMeans/SVM/MLP)
// are intentionally not registered in the decision-only build.
func buildSelectionRegistry(cfg *config.RouterConfig) *selection.Registry {
	modelSelectionCfg := buildModelSelectionConfig(cfg)
	backendModels := cfg.BackendModels
	selectionFactory := selection.NewFactory(modelSelectionCfg)

	if backendModels.ModelConfig != nil {
		selectionFactory = selectionFactory.WithModelConfig(backendModels.ModelConfig)
	}
	if len(cfg.Categories) > 0 {
		selectionFactory = selectionFactory.WithCategories(cfg.Categories)
	}
	embed, defaultEmbeddingConfig := resolveSelectionEmbeddingFunc(cfg)
	selectionFactory = selectionFactory.WithEmbeddingFunc(embed, defaultEmbeddingConfig)

	return selectionFactory.CreateAll()
}

func buildModelSelectionConfig(cfg *config.RouterConfig) *selection.ModelSelectionConfig {
	modelSelectionCfg := &selection.ModelSelectionConfig{
		Method: "static",
	}
	modelSelectionCfg.Elo = buildEloSelectionConfig(cfg)
	modelSelectionCfg.RouterDC = buildRouterDCSelectionConfig(cfg)
	modelSelectionCfg.AutoMix = buildAutoMixSelectionConfig(cfg)
	modelSelectionCfg.Hybrid = buildHybridSelectionConfig(cfg)
	modelSelectionCfg.MultiFactor = selection.DefaultMultiFactorConfig()
	modelSelectionCfg.RLDriven = selection.DefaultRLDrivenConfig()
	modelSelectionCfg.GMTRouter = selection.DefaultGMTRouterConfig()
	return modelSelectionCfg
}

func buildEloSelectionConfig(cfg *config.RouterConfig) *selection.EloConfig {
	eloCfg := cfg.IntelligentRouting.ModelSelection.Elo
	return &selection.EloConfig{
		InitialRating:     eloCfg.InitialRating,
		KFactor:           eloCfg.KFactor,
		CategoryWeighted:  eloCfg.CategoryWeighted,
		DecayFactor:       eloCfg.DecayFactor,
		MinComparisons:    eloCfg.MinComparisons,
		CostScalingFactor: eloCfg.CostScalingFactor,
		StoragePath:       eloCfg.StoragePath,
		AutoSaveInterval:  eloCfg.AutoSaveInterval,
	}
}

func buildRouterDCSelectionConfig(cfg *config.RouterConfig) *selection.RouterDCConfig {
	routerDCCfg := cfg.IntelligentRouting.ModelSelection.RouterDC
	return &selection.RouterDCConfig{
		Temperature:         routerDCCfg.Temperature,
		DimensionSize:       routerDCCfg.DimensionSize,
		MinSimilarity:       routerDCCfg.MinSimilarity,
		UseQueryContrastive: routerDCCfg.UseQueryContrastive,
		UseModelContrastive: routerDCCfg.UseModelContrastive,
		RequireDescriptions: routerDCCfg.RequireDescriptions,
		UseCapabilities:     routerDCCfg.UseCapabilities,
	}
}

func buildAutoMixSelectionConfig(cfg *config.RouterConfig) *selection.AutoMixConfig {
	autoMixCfg := cfg.IntelligentRouting.ModelSelection.AutoMix
	return &selection.AutoMixConfig{
		VerificationThreshold:  autoMixCfg.VerificationThreshold,
		MaxEscalations:         autoMixCfg.MaxEscalations,
		CostAwareRouting:       autoMixCfg.CostAwareRouting,
		CostQualityTradeoff:    autoMixCfg.CostQualityTradeoff,
		DiscountFactor:         autoMixCfg.DiscountFactor,
		UseLogprobVerification: autoMixCfg.UseLogprobVerification,
	}
}

func buildHybridSelectionConfig(cfg *config.RouterConfig) *selection.HybridConfig {
	hybridCfg := cfg.IntelligentRouting.ModelSelection.Hybrid
	return &selection.HybridConfig{
		ExperienceWeight:    hybridCfg.ExperienceWeight,
		RouterDCWeight:      hybridCfg.RouterDCWeight,
		AutoMixWeight:       hybridCfg.AutoMixWeight,
		CostWeight:          hybridCfg.CostWeight,
		QualityGapThreshold: hybridCfg.QualityGapThreshold,
		NormalizeScores:     hybridCfg.NormalizeScores,
	}
}

// resolveSelectionEmbeddingFunc returns the embedding function used by
// selectors that need query embeddings (router_dc, hybrid, ...). Vectors
// always come from the remote OpenAI-compatible endpoint; the built-in
// candle embedding backend has been removed from this build.
func resolveSelectionEmbeddingFunc(cfg *config.RouterConfig) (func(string, selection.EmbeddingConfig) ([]float32, error), selection.EmbeddingConfig) {
	models := cfg.EmbeddingModels
	backend := embedding.BackendOverrideFromEnv()
	if backend == "" {
		backend = models.EmbeddingBackend()
	}
	modelType := selectionEmbeddingModelType(models, backend)
	defaultConfig := selection.EmbeddingConfig{
		ModelType:       modelType,
		TargetDimension: selectionEmbeddingDimension(models, modelType),
	}
	var remoteProvider embedding.Provider
	var remoteError error
	if backend == config.EmbeddingBackendOpenAICompatible {
		remoteProvider, remoteError = embedding.NewProvider(models, embedding.ProviderOptions{})
	}
	return func(text string, embeddingConfig selection.EmbeddingConfig) ([]float32, error) {
		switch backend {
		case config.EmbeddingBackendOpenAICompatible:
			if remoteError != nil {
				return nil, remoteError
			}
			return remoteProvider.Embed(context.Background(), text)
		default:
			return nil, fmt.Errorf("built-in embedding backend %q has been removed from this build; configure backend: %q", backend, config.EmbeddingBackendOpenAICompatible)
		}
	}, defaultConfig
}

func selectionEmbeddingModelType(models config.EmbeddingModels, backend string) string {
	modelType := models.EmbeddingConfig.ModelType
	if modelType != "" {
		return modelType
	}
	if backend == config.EmbeddingBackendOpenAICompatible {
		return config.EmbeddingModelTypeRemote
	}
	return config.EmbeddingModelTypeQwen3
}

func selectionEmbeddingDimension(models config.EmbeddingModels, modelType string) int {
	if models.EmbeddingConfig.TargetDimension > 0 {
		return models.EmbeddingConfig.TargetDimension
	}
	if modelType == config.EmbeddingModelTypeQwen3 {
		return 1024
	}
	return 0
}

func handleDecide(svc *services.ClassificationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req services.DecideRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
			return
		}
		resp, err := svc.DecideModel(req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleReady(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(apiGuide))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

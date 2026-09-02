package classification

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
)

func TestRequestTextEmbeddingCache_DedupsConcurrentResolves(t *testing.T) {
	cache := newRequestTextEmbeddingCache()
	var computes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := cache.resolve("same text", func() ([]float32, error) {
				computes.Add(1)
				return []float32{1, 0, 0}, nil
			})
			if err != nil {
				t.Errorf("resolve returned error: %v", err)
				return
			}
			if len(result) != 3 || result[0] != 1 {
				t.Errorf("unexpected embedding %v", result)
			}
		}()
	}
	wg.Wait()
	if got := computes.Load(); got != 1 {
		t.Fatalf("compute ran %d times for identical text, want 1", got)
	}
}

func TestRequestTextEmbeddingCache_DistinctTextsComputeIndependently(t *testing.T) {
	cache := newRequestTextEmbeddingCache()
	var computes atomic.Int32
	compute := func() ([]float32, error) {
		computes.Add(1)
		return []float32{0.5}, nil
	}
	for _, text := range []string{"a", "b", "a"} {
		if _, err := cache.resolve(text, compute); err != nil {
			t.Fatalf("resolve(%q) failed: %v", text, err)
		}
	}
	if got := computes.Load(); got != 2 {
		t.Fatalf("compute ran %d times for two distinct texts, want 2", got)
	}
}

func TestRequestTextEmbeddingCache_PropagatesErrorToAllCallers(t *testing.T) {
	cache := newRequestTextEmbeddingCache()
	boom := errors.New("provider down")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.resolve("text", func() ([]float32, error) {
				return nil, boom
			}); !errors.Is(err, boom) {
				t.Errorf("err = %v, want provider down", err)
			}
		}()
	}
	wg.Wait()
}

func TestRequestTextEmbeddingCache_NilReceiverFallsThrough(t *testing.T) {
	var computes atomic.Int32
	var cache *requestTextEmbeddingCache
	for i := 0; i < 3; i++ {
		if _, err := cache.resolve("text", func() ([]float32, error) {
			computes.Add(1)
			return []float32{1}, nil
		}); err != nil {
			t.Fatalf("resolve failed: %v", err)
		}
	}
	if got := computes.Load(); got != 3 {
		t.Fatalf("nil cache must not memoize, compute ran %d times", got)
	}
}

// countingTextEmbeddingProvider records how many embedding computations the
// provider performed, so signal-level tests can assert that sibling signals
// sharing one request text collapse into a single remote call.
type countingTextEmbeddingProvider struct {
	calls atomic.Int32
}

func (p *countingTextEmbeddingProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	p.calls.Add(1)
	return []float32{1, 0, 0}, nil
}

func (p *countingTextEmbeddingProvider) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	p.calls.Add(int32(len(texts)))
	results := make([][]float32, len(texts))
	for i := range results {
		results[i] = []float32{1, 0, 0}
	}
	return results, nil
}

func (p *countingTextEmbeddingProvider) Dimension() int { return 3 }

func (p *countingTextEmbeddingProvider) Backend() string { return config.EmbeddingBackendOpenAICompatible }

// TestEvaluateAllSignalsWithContext_DedupsTextEmbeddingAcrossSignals proves
// the orchestrator wiring: with the embedding, complexity, and KB signals all
// enabled against one shared remote provider, a single request evaluation
// performs exactly ONE query-embedding call for the shared request text.
// Construction/preload calls are warmed and excluded from the count.
func TestEvaluateAllSignalsWithContext_DedupsTextEmbeddingAcrossSignals(t *testing.T) {
	provider := &countingTextEmbeddingProvider{}

	embedRules := []config.EmbeddingRule{{
		Name:                      "semantic_topic",
		Candidates:                []string{"topic anchor"},
		SimilarityThreshold:       0.9,
		AggregationMethodConfiged: config.AggregationMethodMax,
	}}
	embedClassifier, err := NewEmbeddingClassifierWithProvider(embedRules, config.HNSWConfig{
		Backend:          config.EmbeddingBackendOpenAICompatible,
		ModelType:        config.EmbeddingModelTypeRemote,
		TargetDimension:  3,
		PreloadEmbeddings: true,
	}, provider)
	if err != nil {
		t.Fatalf("NewEmbeddingClassifierWithProvider failed: %v", err)
	}
	if err := embedClassifier.WarmupCandidateEmbeddings(); err != nil {
		t.Fatalf("WarmupCandidateEmbeddings failed: %v", err)
	}

	complexityRules := []config.ComplexityRule{{
		Name:      "difficulty",
		Threshold: 0.2,
		Hard:      config.ComplexityCandidates{Candidates: []string{"hard anchor"}},
		Easy:      config.ComplexityCandidates{Candidates: []string{"easy anchor"}},
	}}
	complexityClassifier, err := NewComplexityClassifier(
		complexityRules,
		config.EmbeddingModelTypeRemote,
		config.PrototypeScoringConfig{},
		provider,
	)
	if err != nil {
		t.Fatalf("NewComplexityClassifier failed: %v", err)
	}

	kbRoot := writeKnowledgeBaseFixture(t)
	kbClassifier, err := NewKnowledgeBaseClassifierWithProvider(config.KnowledgeBaseConfig{
		Name:      "privacy_kb",
		Source:    config.KnowledgeBaseSource{Path: kbRoot},
		Threshold: 0.5,
	}, config.EmbeddingModelTypeRemote, "", provider)
	if err != nil {
		t.Fatalf("NewKnowledgeBaseClassifierWithProvider failed: %v", err)
	}
	if err := kbClassifier.Preload(); err != nil {
		t.Fatalf("KB Preload failed: %v", err)
	}

	provider.calls.Store(0)

	classifier := &Classifier{
		Config: &config.RouterConfig{
			IntelligentRouting: config.IntelligentRouting{
				Signals: config.Signals{
					EmbeddingRules:  embedRules,
					ComplexityRules: complexityRules,
					KBRules: []config.KBSignalRule{{
						Name:   "privacy",
						KB:     "privacy_kb",
						Target: config.KBSignalTarget{Kind: config.KBTargetKindLabel, Value: "proprietary_code"},
					}},
				},
			},
		},
		keywordEmbeddingClassifier: embedClassifier,
		complexityClassifier:       complexityClassifier,
		kbClassifiers: map[string]*KnowledgeBaseClassifier{
			"privacy_kb": kbClassifier,
		},
	}

	results := classifier.EvaluateAllSignalsWithContext(
		"please review internal code",
		"please review internal code",
		"please review internal code",
		nil,
		nil,
		false,
		true,
		"",
		nil,
		ConversationFacts{},
		"",
	)

	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("remote embedding calls = %d, want 1 (embedding+complexity+KB share one request text)", calls)
	}
	if results == nil || results.Metrics == nil {
		t.Fatalf("expected signal results, got %+v", results)
	}
	if len(results.KBClassifierResults) != 1 {
		t.Fatalf("KB classifier results = %+v, want exactly one KB evaluated", results.KBClassifierResults)
	}
}

// TestEvaluateAllSignalsWithContext_TextCacheDisabledWithoutRemoteProvider
// guards the local-backend path: signals that fall back to local model
// embedding must still evaluate (cache nil-receiver fallthrough) without
// touching the remote provider.
func TestEvaluateAllSignalsWithContext_TextCacheDisabledWithoutRemoteProvider(t *testing.T) {
	provider := &countingTextEmbeddingProvider{}
	embedRules := []config.EmbeddingRule{{
		Name:                      "semantic_topic",
		Candidates:                []string{"topic anchor"},
		SimilarityThreshold:       0.9,
		AggregationMethodConfiged: config.AggregationMethodMax,
	}}
	embedClassifier, err := NewEmbeddingClassifierWithProvider(embedRules, config.HNSWConfig{
		Backend:          config.EmbeddingBackendOpenAICompatible,
		ModelType:        config.EmbeddingModelTypeRemote,
		TargetDimension:  3,
		PreloadEmbeddings: true,
	}, provider)
	if err != nil {
		t.Fatalf("NewEmbeddingClassifierWithProvider failed: %v", err)
	}
	if err := embedClassifier.WarmupCandidateEmbeddings(); err != nil {
		t.Fatalf("WarmupCandidateEmbeddings failed: %v", err)
	}
	provider.calls.Store(0)

	classifier := &Classifier{
		Config: &config.RouterConfig{
			IntelligentRouting: config.IntelligentRouting{
				Signals: config.Signals{
					EmbeddingRules: embedRules,
				},
			},
		},
		keywordEmbeddingClassifier: embedClassifier,
	}

	results := classifier.EvaluateAllSignalsWithContext(
		"topic query",
		"topic query",
		"topic query",
		nil,
		nil,
		false,
		true,
		"",
		nil,
		ConversationFacts{},
		"",
	)

	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("remote embedding calls = %d, want 1 for the single enabled signal", calls)
	}
	if len(results.MatchedEmbeddingRules) == 0 {
		t.Fatalf("expected the embedding signal to evaluate, got no matches")
	}
}

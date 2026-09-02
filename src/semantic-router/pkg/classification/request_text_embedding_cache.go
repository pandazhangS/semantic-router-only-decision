package classification

import (
	"context"
	"sync"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/embedding"
)

// requestTextEmbeddingCache memoizes text embeddings across the lifetime of
// one EvaluateAllSignalsWithContext call. The embedding (semantic rules),
// complexity, and KB signals each independently embed the same request text
// through the shared remote provider; without this cache they would each pay
// a full round-trip for the same input, since their evaluations run in
// independent goroutines and neither sees the other's work.
//
// The cache keys on the raw text only: every text-embedding consumer on this
// path resolves the same shared provider (one endpoint, one model, one
// dimension), so there is no per-consumer variation to fold into the key -
// unlike the image cache, no text consumer asks for a sub-dimension view.
// Local backends (openvino/candle) bypass the cache entirely: their
// embeddings depend on the per-classifier modelType, which is not part of
// the key.
//
// The cache is request-scoped: the orchestrator allocates a fresh one on
// entry and lets it go out of scope on exit, so there is no cross-request
// state to leak or evict. Deliberately no process-level cache: repeated
// identical texts across requests pay the remote call again, which keeps
// resident memory flat.
type requestTextEmbeddingCache struct {
	mu      sync.Mutex
	entries map[string]*requestTextEmbeddingCacheEntry
}

type requestTextEmbeddingCacheEntry struct {
	once      sync.Once
	embedding []float32
	err       error
}

func newRequestTextEmbeddingCache() *requestTextEmbeddingCache {
	return &requestTextEmbeddingCache{
		entries: make(map[string]*requestTextEmbeddingCacheEntry),
	}
}

// resolveProviderTextEmbedding resolves a request-time text embedding through
// the request-scoped cache when one is attached, so sibling signals sharing
// the same request text reuse a single remote provider call.
func resolveProviderTextEmbedding(provider embedding.Provider, text string, cache *requestTextEmbeddingCache) ([]float32, error) {
	if cache == nil {
		return provider.Embed(context.Background(), text)
	}
	return cache.resolve(text, func() ([]float32, error) {
		return provider.Embed(context.Background(), text)
	})
}

// resolve returns the embedding for text, computing it via compute on the
// first call for this text. Concurrent callers for the same text block on
// the same sync.Once and observe the same result.
//
// A nil receiver is treated as cache-disabled: compute runs unconditionally.
// This lets callers outside the orchestrator (tests, single-shot calls)
// share the same call site as cached callers.
func (c *requestTextEmbeddingCache) resolve(text string, compute func() ([]float32, error)) ([]float32, error) {
	if c == nil {
		return compute()
	}
	c.mu.Lock()
	entry, ok := c.entries[text]
	if !ok {
		entry = &requestTextEmbeddingCacheEntry{}
		c.entries[text] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() {
		entry.embedding, entry.err = compute()
	})
	if entry.err != nil {
		return nil, entry.err
	}
	return entry.embedding, nil
}

package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
	"github.com/nauticana/scout/internal/singleflight"
)

// RetrievalCacheScope holds the deployment-owned namespace parts a query
// cannot carry itself; every field is required or the cache is bypassed.
type RetrievalCacheScope struct {
	EmbeddingModelVersion string
	IndexGeneration       string
	PolicyVersion         string
}

// RetrievalCacheKeyer resolves the cache scope for a query, typically from the
// knowledge_base_version row and the retrieval policy in force; an error
// bypasses the cache for that call rather than serving under a guessed key.
type RetrievalCacheKeyer interface {
	Scope(ctx context.Context, query domain.KnowledgeQuery) (RetrievalCacheScope, error)
}

// RetrievalCacheKeyerFunc adapts a function to RetrievalCacheKeyer.
type RetrievalCacheKeyerFunc func(context.Context, domain.KnowledgeQuery) (RetrievalCacheScope, error)

// Scope invokes the function.
func (f RetrievalCacheKeyerFunc) Scope(ctx context.Context, query domain.KnowledgeQuery) (RetrievalCacheScope, error) {
	return f(ctx, query)
}

var _ RetrievalCacheKeyer = RetrievalCacheKeyerFunc(nil)

// CachedRetrieverConfig bounds one in-process retrieval cache.
type CachedRetrieverConfig struct {
	Capacity int
	TTL      time.Duration
	// LoadTimeout bounds a coalesced miss load, which runs detached from any single caller's cancellation.
	LoadTimeout time.Duration
	// Now injects the clock; nil uses time.Now.
	Now func() time.Time
}

// CachedRetriever decorates a KnowledgeRetriever with an entitlement-scoped
// LRU: the key covers tenant, knowledge base and version, TopK, the
// entitlements digest, the query digest, and the keyer's scope, so a hit can
// never cross an authorization, model, index, or policy boundary. Only
// complete results are stored; degraded ones pass through uncached.
type CachedRetriever struct {
	inner  contract.KnowledgeRetriever
	keyer  RetrievalCacheKeyer
	config CachedRetrieverConfig
	cache  *lru.Cache[string, domain.KnowledgeResult]

	flights singleflight.Group[string, domain.KnowledgeResult]

	mu          sync.Mutex
	generations map[retrievalCacheScopeKey]uint64
}

type retrievalCacheScopeKey struct {
	tenantID        int64
	knowledgeBaseID string
}

var _ contract.KnowledgeRetriever = (*CachedRetriever)(nil)

// NewCachedRetriever validates the configuration and wraps inner.
func NewCachedRetriever(inner contract.KnowledgeRetriever, keyer RetrievalCacheKeyer, config CachedRetrieverConfig) (*CachedRetriever, error) {
	if inner == nil || keyer == nil {
		return nil, fmt.Errorf("cached retriever: inner retriever and keyer are required")
	}
	if config.Capacity <= 0 || config.TTL <= 0 || config.LoadTimeout <= 0 {
		return nil, fmt.Errorf("cached retriever: capacity, ttl, and load timeout must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &CachedRetriever{
		inner:       inner,
		keyer:       keyer,
		config:      config,
		cache:       lru.New[string, domain.KnowledgeResult](config.Capacity, config.Now),
		generations: make(map[retrievalCacheScopeKey]uint64),
	}, nil
}

// Retrieve serves a hit, coalesces concurrent misses, and bypasses the cache
// (flagging KnowledgeDegradationCacheBypassed) when no safe key can be formed.
func (retriever *CachedRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	key, err := retriever.key(ctx, query)
	if err != nil {
		result, err := retriever.inner.Retrieve(ctx, query)
		if err != nil {
			return domain.KnowledgeResult{}, err
		}
		result.Degradations = append(result.Degradations, domain.KnowledgeDegradationCacheBypassed)
		return result, nil
	}
	if cached, ok := retriever.cache.Get(key); ok {
		return cloneResult(cached), nil
	}
	result, err := retriever.flights.Do(ctx, key, func(loadCtx context.Context) (domain.KnowledgeResult, error) {
		loadCtx, cancel := context.WithTimeout(loadCtx, retriever.config.LoadTimeout)
		defer cancel()
		result, err := retriever.inner.Retrieve(loadCtx, query)
		if err != nil {
			return domain.KnowledgeResult{}, err
		}
		if len(result.Degradations) == 0 {
			retriever.cache.Set(key, result, retriever.config.TTL)
		}
		return result, nil
	})
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	return cloneResult(result), nil
}

// Invalidate makes every entry of a knowledge base unreachable, e.g. after an
// entitlement change or a new index generation; stale entries age out of the LRU.
func (retriever *CachedRetriever) Invalidate(tenantID int64, knowledgeBaseID string) {
	retriever.mu.Lock()
	retriever.generations[retrievalCacheScopeKey{tenantID, knowledgeBaseID}]++
	retriever.mu.Unlock()
}

func (retriever *CachedRetriever) generation(tenantID int64, knowledgeBaseID string) uint64 {
	retriever.mu.Lock()
	defer retriever.mu.Unlock()
	return retriever.generations[retrievalCacheScopeKey{tenantID, knowledgeBaseID}]
}

func (retriever *CachedRetriever) key(ctx context.Context, query domain.KnowledgeQuery) (string, error) {
	if _, err := authorizeQuery(query); err != nil {
		return "", err
	}
	queryDigest, err := queryDigest(query)
	if err != nil {
		return "", err
	}
	scope, err := retriever.keyer.Scope(ctx, query)
	if err != nil {
		return "", fmt.Errorf("retrieval cache scope: %w", err)
	}
	if strings.TrimSpace(scope.EmbeddingModelVersion) == "" || strings.TrimSpace(scope.IndexGeneration) == "" || strings.TrimSpace(scope.PolicyVersion) == "" {
		return "", fmt.Errorf("%w: retrieval cache scope requires embedding model version, index generation, and policy version", domain.ErrValidation)
	}
	generation := retriever.generation(query.TenantContext.TenantID, query.KnowledgeBaseID)
	parts := []string{
		strconv.FormatInt(query.TenantContext.TenantID, 10), query.KnowledgeBaseID, query.KnowledgeVersion,
		strconv.Itoa(query.TopK), strconv.FormatUint(generation, 10), query.EntitlementsDigest, queryDigest,
		scope.EmbeddingModelVersion, scope.IndexGeneration, scope.PolicyVersion,
	}
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
	}
	return builder.String(), nil
}

func queryDigest(query domain.KnowledgeQuery) (string, error) {
	if len(query.Query) > 0 {
		sum := sha256.Sum256(query.Query)
		return "q:" + hex.EncodeToString(sum[:]), nil
	}
	if len(query.Embedding) == 0 {
		return "", fmt.Errorf("%w: query text or embedding is required", domain.ErrValidation)
	}
	raw := make([]byte, 0, len(query.Embedding)*4)
	for _, value := range query.Embedding {
		var bits [4]byte
		binary.LittleEndian.PutUint32(bits[:], math.Float32bits(value))
		raw = append(raw, bits[:]...)
	}
	sum := sha256.Sum256(raw)
	return "e:" + hex.EncodeToString(sum[:]), nil
}

func cloneResult(result domain.KnowledgeResult) domain.KnowledgeResult {
	clone := result
	clone.Matches = append([]domain.KnowledgeMatch(nil), result.Matches...)
	for index := range clone.Matches {
		clone.Matches[index].Content = append([]byte(nil), result.Matches[index].Content...)
	}
	clone.Degradations = append([]string(nil), result.Degradations...)
	return clone
}

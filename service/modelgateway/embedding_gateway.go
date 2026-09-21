package modelgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GovernedEmbedder is the governed entry point for embedding generation: one
// batch gets the admission, capacity, budget, and settlement a text call gets,
// on the pinned route the tenant's index was built at. It implements
// contract.BatchEmbedder, so knowledge.BatchingEmbedder turns it into the
// EmbeddingGateway the ingest pipeline and the retriever consume.
type GovernedEmbedder struct {
	// Selection is the pinned embedding route; an index cannot be searched with
	// vectors from another one.
	Selection   domain.ModelSelection
	Providers   contract.EmbeddingProviderRegistry
	RateLimiter contract.TenantRateLimiter
	Capacity    contract.CapacityScheduler
	Budgets     contract.TenantBudgetManager
	Pricer      contract.ModelPricer
	// Catalog confirms the tenant may use the route, that it declares
	// CapabilityEmbeddings, and the width it is served at.
	Catalog contract.ModelCandidateCatalog
	// Dimensions pins the vector width when the catalog route declares none.
	Dimensions int
	// RequestID names one batch to the limiter and the budget; nil derives it
	// from the tenant and the batch content digest.
	RequestID func(tenant domain.TenantContext, contents [][]byte) string
	// EstimateTokens sizes a batch for the reservation, and settles a vendor
	// that reports no token count of its own; nil uses EstimatePromptTokens.
	EstimateTokens func([]byte) int64
}

var _ contract.BatchEmbedder = (*GovernedEmbedder)(nil)

// NewGovernedEmbedder validates the required collaborators and pinned route.
func NewGovernedEmbedder(selection domain.ModelSelection, providers contract.EmbeddingProviderRegistry, rateLimiter contract.TenantRateLimiter, capacity contract.CapacityScheduler, budgets contract.TenantBudgetManager, pricer contract.ModelPricer, catalog contract.ModelCandidateCatalog) (*GovernedEmbedder, error) {
	embedder := &GovernedEmbedder{Selection: selection, Providers: providers, RateLimiter: rateLimiter,
		Capacity: capacity, Budgets: budgets, Pricer: pricer, Catalog: catalog}
	if err := embedder.ready(); err != nil {
		return nil, err
	}
	return embedder, nil
}

func (embedder *GovernedEmbedder) ready() error {
	if embedder.Providers == nil || embedder.RateLimiter == nil || embedder.Capacity == nil ||
		embedder.Budgets == nil || embedder.Pricer == nil || embedder.Catalog == nil {
		return fmt.Errorf("governed embedder: provider registry, rate limiter, capacity scheduler, budget manager, pricer, and candidate catalog are required")
	}
	if strings.TrimSpace(embedder.Selection.Provider) == "" || strings.TrimSpace(embedder.Selection.Model) == "" {
		return fmt.Errorf("%w: the embedding route's provider and model are required", domain.ErrValidation)
	}
	if embedder.Dimensions < 0 {
		return fmt.Errorf("%w: dimensions cannot be negative", domain.ErrValidation)
	}
	return nil
}

func (embedder *GovernedEmbedder) reference() domain.ModelReference {
	return selectionReference(embedder.Selection)
}

func (embedder *GovernedEmbedder) estimate(contents [][]byte) int64 {
	var tokens int64
	for _, content := range contents {
		tokens += promptTokens(embedder.EstimateTokens, content)
	}
	return tokens
}

func (embedder *GovernedEmbedder) requestID(tenant domain.TenantContext, contents [][]byte) string {
	if embedder.RequestID != nil {
		return embedder.RequestID(tenant, contents)
	}
	return "embed-" + strconv.FormatInt(tenant.TenantID, 10) + "-" + batchDigest(contents)
}

// batchDigest names one batch by its content, so a retried batch holds the same
// fenced reservation instead of a second one.
func batchDigest(contents [][]byte) string {
	digest := sha256.New()
	for _, content := range contents {
		_, _ = digest.Write([]byte(strconv.Itoa(len(content)) + ":"))
		_, _ = digest.Write(content)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// EmbedBatch embeds one tenant's batch on the pinned route under every control
// a governed model call passes: capability, rate limit, budget, and capacity.
func (embedder *GovernedEmbedder) EmbedBatch(ctx context.Context, tenant domain.TenantContext, contents [][]byte) ([]domain.Embedding, error) {
	if err := embedder.ready(); err != nil {
		return nil, err
	}
	if tenant.TenantID <= 0 || len(contents) == 0 {
		return nil, fmt.Errorf("%w: tenant and at least one input are required", domain.ErrValidation)
	}
	for index, content := range contents {
		if len(content) == 0 {
			return nil, fmt.Errorf("%w: input %d is empty", domain.ErrValidation, index)
		}
	}
	tokens := embedder.estimate(contents)
	requestID := strings.TrimSpace(embedder.requestID(tenant, contents))
	if requestID == "" {
		return nil, fmt.Errorf("%w: request id is required", domain.ErrValidation)
	}
	request := domain.ModelRequest{
		TenantContext: tenant, RequestID: requestID,
		RequiredCapabilities: []string{domain.CapabilityEmbeddings},
		Idempotent:           true,
	}
	candidate, err := capableCandidate(ctx, embedder.Catalog, embedder.Selection, tenant, request.RequiredCapabilities)
	if err != nil {
		return nil, err
	}
	dimensions := embedder.Dimensions
	if candidate.EmbeddingDimensions > 0 {
		dimensions = candidate.EmbeddingDimensions
	}
	provider, err := embedder.Providers.EmbeddingProviderFor(ctx, embedder.Selection)
	if err != nil {
		return nil, err
	}
	if err := embedder.RateLimiter.AllowModelCall(ctx, request); err != nil {
		return nil, err
	}
	budget := modelBudget{budgets: embedder.Budgets, pricer: embedder.Pricer, promptTokens: embedder.EstimateTokens}
	reservation, err := budget.hold(ctx, domain.BudgetRequest{TenantID: tenant.TenantID, RequestID: request.RequestID, Tokens: tokens},
		embedder.reference(), domain.ModelUsage{InputTokens: tokens})
	if err != nil {
		return nil, err
	}
	lease, err := embedder.Capacity.Acquire(ctx, request, embedder.Selection)
	if err != nil {
		return nil, errors.Join(err, budget.settle(ctx, reservation, embedder.reference(), domain.Usage{}))
	}
	embeddings, callErr := provider.Embed(ctx, embedder.Selection, domain.EmbeddingRequest{
		TenantContext: tenant, RequestID: request.RequestID, Inputs: contents, Dimensions: dimensions,
	})
	if callErr == nil {
		callErr = checkEmbeddings(embeddings, contents, dimensions)
	}
	usage, priceErr := budget.priced(ctx, embedder.reference(), spentTokens(embeddings, tokens))
	settleErr := errors.Join(priceErr, lease.Release(context.WithoutCancel(ctx), usage), budget.close(ctx, reservation, usage))
	if callErr != nil {
		return nil, errors.Join(callErr, settleErr)
	}
	return embeddings, settleErr
}

// spentTokens is what the batch spent. A vendor that reports no token count of
// its own is settled at the estimate, and vectors that arrived but failed
// validation are settled too: the provider ran the call either way.
func spentTokens(embeddings []domain.Embedding, estimated int64) domain.Usage {
	if len(embeddings) == 0 {
		return domain.Usage{}
	}
	var reported int64
	for _, embedding := range embeddings {
		reported += embedding.Usage.InputTokens
	}
	if reported <= 0 {
		reported = estimated
	}
	return domain.Usage{InputTokens: reported}
}

// checkEmbeddings holds the provider to one usable vector per input at the
// route's width, so a mismatched vector can never reach an index.
func checkEmbeddings(embeddings []domain.Embedding, contents [][]byte, dimensions int) error {
	if len(embeddings) != len(contents) {
		return fmt.Errorf("%w: %d vectors for %d inputs", domain.ErrInvalidModelOutput, len(embeddings), len(contents))
	}
	for index, embedding := range embeddings {
		switch {
		case len(embedding.Values) == 0:
			return fmt.Errorf("%w: input %d has no vector", domain.ErrInvalidModelOutput, index)
		case dimensions > 0 && len(embedding.Values) != dimensions:
			return fmt.Errorf("%w: input %d has %d dimensions, the route is served at %d", domain.ErrInvalidModelOutput, index, len(embedding.Values), dimensions)
		case embedding.Usage.InputTokens < 0 || embedding.Usage.OutputTokens < 0 || embedding.Usage.ToolCalls < 0 ||
			embedding.Usage.SearchQueries < 0 || embedding.Usage.CostMinorUnits < 0:
			return fmt.Errorf("%w: input %d has invalid usage", domain.ErrInvalidModelOutput, index)
		}
		for _, value := range embedding.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("%w: input %d has a non-finite vector", domain.ErrInvalidModelOutput, index)
			}
		}
	}
	return nil
}

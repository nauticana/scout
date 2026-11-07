package knowledge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/workingset"
)

const (
	qRetrievalBindings = "scout_knowledge_retrieval_bindings"
	sqlModeRetrieval   = "'" + string(domain.KnowledgeModeRetrieval) + "'"

	// DefaultCandidateTopK is the matches one retrieval binding offers a turn when ReleaseCandidates.TopK is zero.
	DefaultCandidateTopK = 8

	pinnedAuthorityRank    = 0
	retrievedAuthorityRank = 1
)

var releaseCandidateQueries = map[string]string{
	qRetrievalBindings: `
SELECT knowledge_base_id, knowledge_version FROM agent_knowledge_binding
 WHERE partner_id = ? AND agent_id = ? AND agent_version = ? AND mode_code = ` + sqlModeRetrieval + `
 ORDER BY knowledge_base_id`,
}

// ReleaseCandidates offers a turn the knowledge its pinned release binds: every retrieval binding's
// matches for the task, better matches more reliable, and, with Pinned, each document bound whole as
// a required candidate, kept or the compilation fails. A caller with no entitlements retrieves
// nothing. Compose it instead of ReleaseLoopRequestBuilder.Knowledge.
type ReleaseCandidates struct {
	DB           keelport.DatabaseRepository
	Retriever    contract.KnowledgeRetriever
	Pinned       contract.PinnedKnowledgeResolver
	Entitlements contract.EntitlementResolver
	// TopK bounds the matches of one binding; zero takes DefaultCandidateTopK.
	TopK int
	// Budget bounds one binding's retrieval; zero leaves it to the turn's deadline.
	Budget time.Duration

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.WorkingSetCandidateSource = (*ReleaseCandidates)(nil)

func (source *ReleaseCandidates) Candidates(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
	if source.DB == nil || source.Entitlements == nil {
		return domain.WorkingSetCandidates{}, fmt.Errorf("%w: release candidates need a database and an entitlement resolver", domain.ErrNotReady)
	}
	if source.TopK < 0 || source.Budget < 0 {
		return domain.WorkingSetCandidates{}, fmt.Errorf("%w: release candidate bounds cannot be negative", domain.ErrValidation)
	}
	source.once.Do(func() { source.qs = source.DB.GetQueryService(ctx, releaseCandidateQueries) })
	labels, digest, err := source.Entitlements.Entitlements(ctx, request.Principal)
	if errors.Is(err, domain.ErrForbidden) {
		return domain.WorkingSetCandidates{}, nil
	}
	if err != nil {
		return domain.WorkingSetCandidates{}, err
	}
	var offered domain.WorkingSetCandidates
	if source.Pinned != nil {
		pinned, err := source.Pinned.PinnedKnowledge(ctx, domain.PinnedKnowledgeRequest{
			TenantContext: request.TenantContext, AgentID: request.Principal.ID, AgentVersion: request.Principal.Release, Principal: request.Principal,
			Entitlements: labels, EntitlementsDigest: digest,
		})
		if err != nil {
			return domain.WorkingSetCandidates{}, err
		}
		for _, document := range pinned.Documents {
			offered.Candidates = append(offered.Candidates, pinnedCandidate(document))
		}
	}
	if len(labels) == 0 {
		return offered, nil
	}
	bindings, err := source.qs.Query(ctx, qRetrievalBindings, request.TenantContext.TenantID, request.Principal.ID, request.Principal.Release)
	if err != nil {
		return domain.WorkingSetCandidates{}, fmt.Errorf("list retrieval bindings of %s@%s: %w", request.Principal.ID, request.Principal.Release, err)
	}
	if len(bindings.Rows) > 0 && source.Retriever == nil {
		return domain.WorkingSetCandidates{}, fmt.Errorf("%w: release %s@%s binds retrieval but no retriever is composed", domain.ErrNotReady, request.Principal.ID, request.Principal.Release)
	}
	if err = bindings.RequireColumns(2); err != nil {
		return domain.WorkingSetCandidates{}, fmt.Errorf("list retrieval bindings of %s@%s: %w", request.Principal.ID, request.Principal.Release, err)
	}
	for _, row := range bindings.Rows {
		base, version := common.AsString(row[0]), common.AsString(row[1])
		result, err := source.Retriever.Retrieve(ctx, domain.KnowledgeQuery{
			TenantContext: request.TenantContext, RequestID: request.RequestID, ConversationID: request.ConversationID,
			KnowledgeBaseID: base, KnowledgeVersion: version, Principal: request.Principal, Entitlements: labels, EntitlementsDigest: digest,
			Query: []byte(request.Task), TopK: cmp.Or(source.TopK, DefaultCandidateTopK), Budget: source.Budget,
		})
		if err != nil {
			return domain.WorkingSetCandidates{}, fmt.Errorf("retrieve from knowledge base %q: %w", base, err)
		}
		for position, match := range result.Matches {
			offered.Candidates = append(offered.Candidates, matchCandidate(base, version, position, match))
		}
	}
	return offered, nil
}

func pinnedCandidate(document domain.PinnedDocument) domain.WorkingSetCandidate {
	return domain.WorkingSetCandidate{
		ID:            candidateID(document.KnowledgeBaseID, document.KnowledgeVersion, document.DocumentID),
		Content:       string(document.Content),
		Provenance:    knowledgeProvenance(document.KnowledgeBaseID, document.KnowledgeVersion, document.SourceURI, document.SourceVersion),
		AuthorityRank: pinnedAuthorityRank, Standing: domain.StandingCurrent, ReliabilityPercent: domain.MaxWorkingSetReliability,
		Required: true, TokenCost: int64(document.TokenCount),
	}
}

// Retrieval reads only a version's active, untombstoned chunks, so a match is current.
func matchCandidate(base, version string, position int, match domain.KnowledgeMatch) domain.WorkingSetCandidate {
	content := string(match.Content)
	return domain.WorkingSetCandidate{
		ID:            candidateID(base, version, fmt.Sprintf("%s#%d", match.DocumentID, match.ChunkNo)),
		Content:       content,
		Provenance:    knowledgeProvenance(base, version, match.SourceURI, match.SourceVersion),
		AuthorityRank: retrievedAuthorityRank, Standing: domain.StandingCurrent,
		ReliabilityPercent: max(domain.MaxWorkingSetReliability-position, 0), TokenCost: workingset.EstimateTokens(content),
	}
}

func knowledgeProvenance(base, version, sourceURI, sourceVersion string) string {
	return fmt.Sprintf("knowledge %s@%s: %s@%s", base, version, sourceURI, sourceVersion)
}

// candidateID falls back to a digest past the width of turn_working_set_item.candidate_id.
func candidateID(base, version, item string) string {
	id := "knowledge:" + base + "@" + version + ":" + item
	if len(id) <= domain.MaxWorkingSetCandidateIDBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "knowledge:" + hex.EncodeToString(sum[:])
}

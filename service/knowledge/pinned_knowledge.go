package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPinnedBindings = "scout_knowledge_pinned_bindings"
	qPinnedChunks   = "scout_knowledge_pinned_chunks"
)

// The chunk scan reads no vector table, so knowledge is read whole without
// knowledge_vector installed. Version, manifest and tombstone scope the scan;
// the any-of entitlement rule retrieval compiles into SQL is applied in Go
// before any content is loaded, which keeps the query dialect-neutral.
var pinnedKnowledgeQueries = map[string]string{
	qPinnedBindings: `
SELECT b.knowledge_base_id, b.knowledge_version, b.max_whole_tokens, d.document_id
  FROM agent_knowledge_binding b
  LEFT JOIN agent_knowledge_document d
    ON d.tenant_id = b.tenant_id AND d.agent_id = b.agent_id AND d.agent_version = b.agent_version
   AND d.knowledge_base_id = b.knowledge_base_id
 WHERE b.tenant_id = ? AND b.agent_id = ? AND b.agent_version = ? AND b.mode_code = 'whole'
 ORDER BY b.knowledge_base_id, d.ordinal, d.document_id`,
	qPinnedChunks: `
SELECT c.document_id, c.chunk_no, c.content_uri, c.token_count, d.source_uri, c.source_version,
       c.start_offset, c.end_offset, c.entitlements
  FROM knowledge_chunk c
  JOIN knowledge_document d
    ON d.tenant_id = c.tenant_id AND d.knowledge_base_id = c.knowledge_base_id
   AND d.knowledge_version = c.knowledge_version AND d.document_id = c.document_id
  JOIN knowledge_document_manifest m
    ON m.tenant_id = c.tenant_id AND m.knowledge_base_id = c.knowledge_base_id
   AND m.document_id = c.document_id AND m.active_version = c.knowledge_version AND m.tombstoned = FALSE
 WHERE c.tenant_id = ? AND c.knowledge_base_id = ? AND c.knowledge_version = ?
 ORDER BY c.document_id, c.chunk_no`,
}

// maxAssemblyOverlap bounds the repeated context one chunk may carry from the
// previous one; the chunker's overlap is a fraction of a chunk.
const maxAssemblyOverlap = 8192

// TablePinnedKnowledge resolves the documents an agent version binds whole:
// the chunks of the pinned knowledge version, authorized and redacted as
// ingested, reassembled in order into one document text. A binding that
// exceeds its token ceiling fails; nothing is silently truncated.
type TablePinnedKnowledge struct {
	DB keelport.DatabaseRepository
	// Content fetches one chunk's stored bytes by its content URI.
	Content contract.SourceLoader

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.PinnedKnowledgeResolver = (*TablePinnedKnowledge)(nil)

func (resolver *TablePinnedKnowledge) init(ctx context.Context) error {
	if resolver.DB == nil || resolver.Content == nil {
		return fmt.Errorf("pinned knowledge: database and chunk content loader are required")
	}
	resolver.once.Do(func() { resolver.qs = resolver.DB.GetQueryService(ctx, pinnedKnowledgeQueries) })
	if resolver.qs == nil {
		return fmt.Errorf("pinned knowledge: query service is required")
	}
	return nil
}

// wholeBinding is one whole-mode binding and the documents it names; no named
// document means every document of the bound version.
type wholeBinding struct {
	knowledgeBaseID  string
	knowledgeVersion string
	maxTokens        int64
	documents        []string
}

// PinnedKnowledge assembles every whole-mode binding of the agent version.
func (resolver *TablePinnedKnowledge) PinnedKnowledge(ctx context.Context, request domain.PinnedKnowledgeRequest) (domain.PinnedKnowledge, error) {
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.AgentVersion) == "" {
		return domain.PinnedKnowledge{}, fmt.Errorf("%w: tenant, agent, and agent version are required", domain.ErrValidation)
	}
	if len(request.Entitlements) == 0 {
		return domain.PinnedKnowledge{}, fmt.Errorf("%w: resolved entitlements are required", domain.ErrValidation)
	}
	held, err := ParseEntitlements(request.Entitlements)
	if err != nil {
		return domain.PinnedKnowledge{}, err
	}
	if err := resolver.init(ctx); err != nil {
		return domain.PinnedKnowledge{}, err
	}
	bindings, err := resolver.bindings(ctx, request)
	if err != nil {
		return domain.PinnedKnowledge{}, err
	}
	var pinned domain.PinnedKnowledge
	for _, binding := range bindings {
		documents, err := resolver.documents(ctx, request.TenantContext, held, binding)
		if err != nil {
			return domain.PinnedKnowledge{}, err
		}
		var bindingTokens int64
		for _, document := range documents {
			bindingTokens += int64(document.TokenCount)
		}
		if binding.maxTokens > 0 && bindingTokens > binding.maxTokens {
			return domain.PinnedKnowledge{}, fmt.Errorf("%w: knowledge base %q binds %d tokens whole, above its ceiling of %d",
				domain.ErrExecutionLimit, binding.knowledgeBaseID, bindingTokens, binding.maxTokens)
		}
		pinned.Documents = append(pinned.Documents, documents...)
		pinned.TokenCount += int(bindingTokens)
	}
	return pinned, nil
}

func (resolver *TablePinnedKnowledge) bindings(ctx context.Context, request domain.PinnedKnowledgeRequest) ([]wholeBinding, error) {
	res, err := resolver.qs.Query(ctx, qPinnedBindings, request.TenantContext.TenantID, request.AgentID, request.AgentVersion)
	if err != nil {
		return nil, fmt.Errorf("list whole knowledge bindings of %s@%s: %w", request.AgentID, request.AgentVersion, err)
	}
	var bindings []wholeBinding
	for _, row := range res.Rows {
		binding := wholeBinding{
			knowledgeBaseID:  strings.TrimSpace(common.AsString(row[0])),
			knowledgeVersion: strings.TrimSpace(common.AsString(row[1])),
			maxTokens:        max(0, common.AsInt64(row[2])),
		}
		documentID := strings.TrimSpace(common.AsString(row[3]))
		if last := len(bindings) - 1; last >= 0 && bindings[last].knowledgeBaseID == binding.knowledgeBaseID {
			if documentID != "" {
				bindings[last].documents = append(bindings[last].documents, documentID)
			}
			continue
		}
		if documentID != "" {
			binding.documents = []string{documentID}
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// chunkRow is one authorized chunk of a pinned document.
type chunkRow struct {
	documentID    string
	contentURI    string
	sourceURI     string
	sourceVersion string
	tokens        int
	startOffset   int
	endOffset     int
}

// documents assembles one binding's documents in the order it names them.
func (resolver *TablePinnedKnowledge) documents(ctx context.Context, tenant domain.TenantContext, held []string, binding wholeBinding) ([]domain.PinnedDocument, error) {
	res, err := resolver.qs.Query(ctx, qPinnedChunks, tenant.TenantID, binding.knowledgeBaseID, binding.knowledgeVersion)
	if err != nil {
		return nil, fmt.Errorf("read knowledge base %q whole: %w", binding.knowledgeBaseID, err)
	}
	chunks := make(map[string][]chunkRow)
	var order []string
	wanted := make(map[string]struct{}, len(binding.documents))
	for _, documentID := range binding.documents {
		wanted[documentID] = struct{}{}
	}
	for _, row := range res.Rows {
		documentID := strings.TrimSpace(common.AsString(row[0]))
		if len(wanted) > 0 {
			if _, ok := wanted[documentID]; !ok {
				continue
			}
		}
		granted, err := ParseEntitlements([]byte(common.AsString(row[8])))
		if err != nil {
			return nil, fmt.Errorf("chunk %v of document %q: %w", row[1], documentID, err)
		}
		if !Entitled(granted, held) {
			continue
		}
		chunk := chunkRow{
			documentID:    documentID,
			contentURI:    strings.TrimSpace(common.AsString(row[2])),
			tokens:        int(common.AsInt64(row[3])),
			sourceURI:     strings.TrimSpace(common.AsString(row[4])),
			sourceVersion: strings.TrimSpace(common.AsString(row[5])),
			startOffset:   int(common.AsInt64(row[6])),
			endOffset:     int(common.AsInt64(row[7])),
		}
		if _, seen := chunks[chunk.documentID]; !seen {
			order = append(order, chunk.documentID)
		}
		chunks[chunk.documentID] = append(chunks[chunk.documentID], chunk)
	}
	if len(binding.documents) > 0 {
		order = binding.documents
	}
	documents := make([]domain.PinnedDocument, 0, len(order))
	for _, documentID := range order {
		rows := chunks[documentID]
		if len(rows) == 0 {
			return nil, fmt.Errorf("%w: document %q of knowledge base %q is bound whole but no authorized chunk of version %q remains",
				domain.ErrNotFound, documentID, binding.knowledgeBaseID, binding.knowledgeVersion)
		}
		document, err := resolver.assemble(ctx, tenant, binding, rows)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, nil
}

// assemble concatenates a document's chunks, dropping the context each chunk
// repeats from the one before it.
func (resolver *TablePinnedKnowledge) assemble(ctx context.Context, tenant domain.TenantContext, binding wholeBinding, rows []chunkRow) (domain.PinnedDocument, error) {
	document := domain.PinnedDocument{
		KnowledgeBaseID: binding.knowledgeBaseID, KnowledgeVersion: binding.knowledgeVersion,
		DocumentID: rows[0].documentID, SourceURI: rows[0].sourceURI, SourceVersion: rows[0].sourceVersion,
	}
	var content []byte
	previousEnd := -1
	for _, row := range rows {
		if row.contentURI == "" {
			return domain.PinnedDocument{}, fmt.Errorf("%w: a chunk of document %q has no stored content", domain.ErrNotFound, row.documentID)
		}
		chunk, err := resolver.Content.Load(ctx, domain.KnowledgeDocument{
			TenantContext: tenant, KnowledgeBaseID: binding.knowledgeBaseID,
			KnowledgeVersion: binding.knowledgeVersion, DocumentID: row.documentID, SourceURI: row.contentURI,
		})
		if err != nil {
			return domain.PinnedDocument{}, fmt.Errorf("load chunk of document %q: %w", row.documentID, err)
		}
		overlap := 0
		switch {
		case previousEnd > row.startOffset:
			overlap = repeatedPrefix(content, chunk)
		case previousEnd >= 0 && previousEnd < row.startOffset:
			// The bytes between two sections belong to no chunk; keep them apart.
			content = append(content, '\n')
		}
		content = append(content, chunk[overlap:]...)
		previousEnd = row.endOffset
		document.TokenCount += row.tokens
	}
	document.Content = content
	return document, nil
}

// repeatedPrefix is the length of the longest prefix of next that the tail of
// assembled already ends with — the chunker's overlap window, matched on the
// stored bytes so redaction cannot shift it.
func repeatedPrefix(assembled, next []byte) int {
	longest := min(len(assembled), len(next), maxAssemblyOverlap)
	for length := longest; length > 0; length-- {
		if bytes.HasSuffix(assembled, next[:length]) {
			return length
		}
	}
	return 0
}

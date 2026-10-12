package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qMemorySections = "scout_memory_sections"
	qMemoryAppend   = "scout_memory_append"

	// sectionChars is the agent_memory_section_max_chars default.
	sectionChars = 8000
	// linePrefix is the characters of the dated bullet "- 2006-01-02 — " the store writes ahead of a fact.
	linePrefix = 15
)

var memoryQueries = map[string]string{
	qMemorySections: `
SELECT s.id, s.caption, m.body, m.updated_at
  FROM agent_memory m
  JOIN memory_section s ON s.id = m.section_id
 WHERE m.partner_id = ? AND m.body <> ''
 ORDER BY s.display_order, s.id`,
	// The oldest lines give way once a section is full; a cut never leaves half a line.
	qMemoryAppend: `
INSERT INTO agent_memory (partner_id, section_id, body, updated_by)
SELECT ?, s.id, '- ' || to_char(CURRENT_DATE, 'YYYY-MM-DD') || ' — ' || ? || chr(10), ?
  FROM memory_section s
 WHERE s.id = ? AND (s.agent_writable OR ?)
ON CONFLICT (partner_id, section_id) DO UPDATE
   SET body = CASE WHEN char_length(agent_memory.body || EXCLUDED.body) <= ? THEN agent_memory.body || EXCLUDED.body
                   ELSE substr(RIGHT(agent_memory.body || EXCLUDED.body, ?), strpos(RIGHT(agent_memory.body || EXCLUDED.body, ?), chr(10)) + 1) END,
       updated_at = CURRENT_TIMESTAMP, updated_by = EXCLUDED.updated_by
RETURNING section_id`,
}

// TableStore keeps tenant memory in agent_memory, one body per memory_section.
type TableStore struct {
	DB keelport.DatabaseRepository
	// MaxSectionChars bounds one section's body (agent_memory_section_max_chars); 0 takes 8000.
	MaxSectionChars int

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.MemoryStore = (*TableStore)(nil)

func (store *TableStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("memory store: database is required")
	}
	if store.MaxSectionChars < 0 {
		return fmt.Errorf("%w: memory section bound cannot be negative", domain.ErrValidation)
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, memoryQueries) })
	if store.qs == nil {
		return fmt.Errorf("memory store: query service is required")
	}
	return nil
}

func (store *TableStore) Sections(ctx context.Context, tenantID int64) ([]domain.MemorySection, error) {
	if tenantID <= 0 {
		return nil, fmt.Errorf("%w: memory needs a tenant", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qMemorySections, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read memory: %w", err)
	}
	sections := make([]domain.MemorySection, 0, len(result.Rows))
	for _, row := range result.Rows {
		sections = append(sections, domain.MemorySection{
			SectionID: common.AsString(row[0]), Caption: common.AsString(row[1]),
			Body: common.AsString(row[2]), UpdatedAt: common.AsTime(row[3]),
		})
	}
	return sections, nil
}

func (store *TableStore) Append(ctx context.Context, fact domain.MemoryFact) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	max := store.MaxSectionChars
	if max == 0 {
		max = sectionChars
	}
	// One fact is one line, so it can neither forge provenance nor split on trimming.
	text := strings.Join(strings.Fields(fact.Fact), " ")
	actor := strings.Join(strings.Fields(fact.Actor), " ")
	var vouch string
	switch fact.Source {
	case domain.MemoryStated:
		vouch = "stated by "
	case domain.MemoryVerified:
		vouch = "verified by "
	default:
		return domain.ErrUnprovenFact
	}
	switch {
	case fact.TenantID <= 0 || strings.TrimSpace(fact.SectionID) == "":
		return fmt.Errorf("%w: a remembered fact needs a tenant and a section", domain.ErrValidation)
	case text == "" || actor == "" || utf8.RuneCountInString(actor) > 80:
		return fmt.Errorf("%w: a remembered fact needs text and an actor of at most 80 characters", domain.ErrValidation)
	}
	line := text + " (" + vouch + actor + ")"
	if linePrefix+utf8.RuneCountInString(line)+1 > max {
		return fmt.Errorf("%w: a remembered fact must fit in %d characters with its date and provenance", domain.ErrValidation, max)
	}
	result, err := store.qs.Query(ctx, qMemoryAppend, fact.TenantID, line, actor, fact.SectionID,
		fact.Source == domain.MemoryStated, max, max, max)
	if err != nil {
		return fmt.Errorf("append memory: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: section %q is unknown or agents may not write it", domain.ErrForbidden, fact.SectionID)
	}
	return nil
}

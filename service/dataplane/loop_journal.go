package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qLoopEntryList   = "scout_step_loop_entry_list"
	qLoopEntryInsert = "scout_step_loop_entry_insert"
	qLoopEntryGet    = "scout_step_loop_entry_get"

	// maxLoopEntries bounds one journal read; a loop is limited well below it.
	maxLoopEntries = 4096
)

var loopJournalQueries = map[string]string{
	qLoopEntryList: `
SELECT entry_no, entry_uri, entry_digest
  FROM step_loop_entry
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
 ORDER BY entry_no
 LIMIT ?`,
	qLoopEntryInsert: `
INSERT INTO step_loop_entry (tenant_id, request_id, execution_step_id, entry_no, entry_uri, entry_digest)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING
RETURNING entry_no`,
	qLoopEntryGet: `
SELECT entry_no, entry_uri, entry_digest
  FROM step_loop_entry
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ? AND entry_no = ?`,
}

func validateLoopAppend(key domain.LoopKey, entry domain.LoopEntry) error {
	if key.TenantID <= 0 || strings.TrimSpace(key.RequestID) == "" || key.ExecutionStepID <= 0 {
		return fmt.Errorf("%w: loop journal key needs tenant, request id, and execution step id", domain.ErrValidation)
	}
	if entry.EntryNo <= 0 || entry.Kind == "" {
		return fmt.Errorf("%w: loop journal entry needs a positive number and a kind", domain.ErrValidation)
	}
	return nil
}

// TableLoopJournal keeps loop entries in step_loop_entry. Entry bodies live in
// object storage; the row keeps URI and digest, and the primary key makes the
// first writer of an entry number win.
type TableLoopJournal struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec

	once sync.Once
	qs   port.QueryService
}

func (journal *TableLoopJournal) init(ctx context.Context) error {
	if journal.DB == nil || journal.Objects == nil {
		return fmt.Errorf("%w: loop journal needs a database and an object codec", domain.ErrValidation)
	}
	journal.once.Do(func() { journal.qs = journal.DB.GetQueryService(ctx, loopJournalQueries) })
	if journal.qs == nil {
		return fmt.Errorf("loop journal: query service is required")
	}
	return nil
}

func (journal *TableLoopJournal) Load(ctx context.Context, key domain.LoopKey) ([]domain.LoopEntry, error) {
	if err := journal.init(ctx); err != nil {
		return nil, err
	}
	result, err := journal.qs.Query(ctx, qLoopEntryList, key.TenantID, key.RequestID, key.ExecutionStepID, maxLoopEntries)
	if err != nil {
		return nil, fmt.Errorf("list loop entries: %w", err)
	}
	entries := make([]domain.LoopEntry, 0, len(result.Rows))
	for index, row := range result.Rows {
		entry, err := journal.hydrate(ctx, row)
		if err != nil {
			return nil, err
		}
		if entry.EntryNo != index+1 {
			return nil, fmt.Errorf("%w: loop journal of %q has a gap at entry %d", domain.ErrConflict, key.RequestID, index+1)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (journal *TableLoopJournal) Append(ctx context.Context, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if err := journal.init(ctx); err != nil {
		return domain.LoopEntry{}, err
	}
	if err := validateLoopAppend(key, entry); err != nil {
		return domain.LoopEntry{}, err
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("encode loop entry: %w", err)
	}
	ref, err := journal.Objects.Dehydrate(ctx, loopEntryName(key, entry.EntryNo), payload)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("dehydrate loop entry: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	inserted, err := journal.qs.Query(ctx, qLoopEntryInsert, key.TenantID, key.RequestID, key.ExecutionStepID, entry.EntryNo, ref.URI, ref.Digest)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("insert loop entry: %w", err)
	}
	if len(inserted.Rows) > 0 {
		return entry, nil
	}
	existing, err := journal.qs.Query(ctx, qLoopEntryGet, key.TenantID, key.RequestID, key.ExecutionStepID, entry.EntryNo)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("read loop entry: %w", err)
	}
	if len(existing.Rows) == 0 {
		return domain.LoopEntry{}, fmt.Errorf("loop entry %d of %q disappeared after insert", entry.EntryNo, key.RequestID)
	}
	if common.AsString(existing.Rows[0][1]) != ref.URI {
		if err := journal.Objects.Delete(ctx, ref); err != nil {
			return domain.LoopEntry{}, fmt.Errorf("discard superseded loop entry: %w", err)
		}
	}
	return journal.hydrate(ctx, existing.Rows[0])
}

func (journal *TableLoopJournal) hydrate(ctx context.Context, row []any) (domain.LoopEntry, error) {
	payload, err := journal.Objects.Hydrate(ctx, domain.ObjectRef{URI: common.AsString(row[1]), Digest: common.AsString(row[2])})
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("hydrate loop entry %d: %w", common.AsInt64(row[0]), err)
	}
	var entry domain.LoopEntry
	if err := json.Unmarshal(payload, &entry); err != nil {
		return domain.LoopEntry{}, fmt.Errorf("decode loop entry %d: %w", common.AsInt64(row[0]), err)
	}
	return entry, nil
}

func loopEntryName(key domain.LoopKey, entryNo int) string {
	return fmt.Sprintf("loop/%d/%s/%d/%d", key.TenantID, url.PathEscape(key.RequestID), key.ExecutionStepID, entryNo)
}

// MemoryLoopJournal is the single-process journal for tests and embedded runtimes.
type MemoryLoopJournal struct {
	mu      sync.Mutex
	entries map[domain.LoopKey][]domain.LoopEntry
}

func (journal *MemoryLoopJournal) Load(_ context.Context, key domain.LoopKey) ([]domain.LoopEntry, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]domain.LoopEntry(nil), journal.entries[key]...), nil
}

func (journal *MemoryLoopJournal) Append(_ context.Context, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if err := validateLoopAppend(key, entry); err != nil {
		return domain.LoopEntry{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	stored := journal.entries[key]
	switch {
	case entry.EntryNo <= len(stored):
		return stored[entry.EntryNo-1], nil
	case entry.EntryNo != len(stored)+1:
		return domain.LoopEntry{}, fmt.Errorf("%w: loop journal entry %d would leave a gap", domain.ErrConflict, entry.EntryNo)
	}
	if journal.entries == nil {
		journal.entries = make(map[domain.LoopKey][]domain.LoopEntry)
	}
	journal.entries[key] = append(stored, entry)
	return entry, nil
}

var (
	_ contract.LoopJournal = (*TableLoopJournal)(nil)
	_ contract.LoopJournal = (*MemoryLoopJournal)(nil)
)

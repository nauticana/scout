// Package confirmationtest runs confirmation.Executor without a database: an
// in-memory store with the guards of confirmation.TableStore, and the suite
// every contract.MCPConfirmationStore must pass.
package confirmationtest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/confirmation"
)

// Store keeps confirmations in memory. Now is the store clock; nil uses time.Now.
type Store struct {
	Now func() time.Time

	mu     sync.Mutex
	rows   map[int64]*domain.MCPConfirmation
	leases map[int64]time.Time
	next   int64
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) find(key domain.MCPConfirmationKey) *domain.MCPConfirmation {
	if row := s.rows[key.ID]; row != nil && row.TenantID == key.TenantID {
		return row
	}
	return nil
}

func cloneJSON(value json.RawMessage) json.RawMessage { return slices.Clone(value) }

func conflict(verb string, key domain.MCPConfirmationKey) error {
	return fmt.Errorf("%w: cannot %s confirmation %d in its current state", domain.ErrConflict, verb, key.ID)
}

// Prepare records a pending confirmation, or returns the maker's own open one
// for the identical action with created false.
func (s *Store) Prepare(_ context.Context, draft domain.MCPConfirmationDraft) (domain.MCPConfirmation, bool, error) {
	requirements, err := confirmation.ValidateDraft(draft)
	if err != nil {
		return domain.MCPConfirmation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, row := range s.rows {
		if open(row.Status) && row.TenantID == draft.TenantID && row.Tool == draft.Tool && row.PayloadDigest == draft.Digest {
			if row.Maker != draft.Maker || row.ClientRef != draft.ClientRef {
				return domain.MCPConfirmation{}, false, fmt.Errorf("%w: an identical action is already waiting for another maker", domain.ErrConflict)
			}
			return s.view(row, now), false, nil
		}
	}
	if s.rows == nil {
		s.rows, s.leases = map[int64]*domain.MCPConfirmation{}, map[int64]time.Time{}
	}
	s.next++
	s.rows[s.next] = &domain.MCPConfirmation{ID: s.next, TenantID: draft.TenantID, Maker: draft.Maker, ClientRef: draft.ClientRef,
		Tool: draft.Tool, Payload: cloneJSON(draft.Payload), PayloadDigest: draft.Digest, Preview: cloneJSON(draft.Preview), Requirements: cloneJSON(requirements),
		Channel: draft.Channel, Status: domain.MCPConfirmationPending, ApprovalRequired: draft.ApprovalRequired,
		ExpiresAt: now.Add(wholeSeconds(draft.TTL)), CreatedAt: now}
	return s.view(s.rows[s.next], now), true, nil
}

func open(status domain.MCPConfirmationStatus) bool {
	switch status {
	case domain.MCPConfirmationPending, domain.MCPConfirmationApproved, domain.MCPConfirmationExecuting, domain.MCPConfirmationUnknown:
		return true
	}
	return false
}

func (s *Store) view(row *domain.MCPConfirmation, now time.Time) domain.MCPConfirmation {
	out := *row
	out.Payload, out.Preview = cloneJSON(row.Payload), cloneJSON(row.Preview)
	out.Requirements, out.Result = cloneJSON(row.Requirements), cloneJSON(row.Result)
	out.Expired = row.Status == domain.MCPConfirmationPending && !row.ExpiresAt.After(now)
	return out
}

// Get reads one confirmation inside its tenant.
func (s *Store) Get(_ context.Context, key domain.MCPConfirmationKey) (domain.MCPConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.find(key)
	if row == nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	return s.view(row, s.now()), nil
}

// Decide records a decision on an unexpired pending confirmation no maker-checker request owns.
func (s *Store) Decide(_ context.Context, key domain.MCPConfirmationKey, decider domain.PrincipalRef, approve bool, note string) error {
	if decider.Kind == "" || strings.TrimSpace(decider.ID) == "" {
		return fmt.Errorf("%w: decider is required", domain.ErrPrincipalUnknown)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	row := s.find(key)
	if row == nil || row.Status != domain.MCPConfirmationPending || row.ApprovalRequired || row.ApprovalID != 0 || !row.ExpiresAt.After(now) {
		return conflict("decide", key)
	}
	row.Status, row.Decider, row.DecisionNote, row.DecidedAt = domain.MCPConfirmationDeclined, decider, truncate(note), now
	if approve {
		row.Status = domain.MCPConfirmationApproved
	}
	return nil
}

// Approved lists approved confirmations, oldest decision first.
func (s *Store) Approved(_ context.Context, limit int) ([]domain.MCPConfirmationKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var approved []*domain.MCPConfirmation
	for _, row := range s.rows {
		if row.Status == domain.MCPConfirmationApproved {
			approved = append(approved, row)
		}
	}
	slices.SortFunc(approved, func(a, b *domain.MCPConfirmation) int {
		return cmp.Or(a.DecidedAt.Compare(b.DecidedAt), cmp.Compare(a.ID, b.ID))
	})
	limit = batch(limit)
	keys := make([]domain.MCPConfirmationKey, 0, min(limit, len(approved)))
	for _, row := range approved[:min(limit, len(approved))] {
		keys = append(keys, domain.MCPConfirmationKey{TenantID: row.TenantID, ID: row.ID})
	}
	return keys, nil
}

// Claim moves an approved confirmation to executing under a new fence.
func (s *Store) Claim(_ context.Context, key domain.MCPConfirmationKey, lease time.Duration) (int64, bool, error) {
	if err := validLease(lease); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.find(key)
	if row == nil || row.Status != domain.MCPConfirmationApproved {
		return 0, false, nil
	}
	row.Status = domain.MCPConfirmationExecuting
	row.Fence++
	row.Attempts++
	s.leases[row.ID] = s.now().Add(wholeSeconds(lease))
	return row.Fence, true, nil
}

// Renew extends a claim whose fence still holds.
func (s *Store) Renew(_ context.Context, key domain.MCPConfirmationKey, fence int64, lease time.Duration) error {
	if err := validLease(lease); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.find(key)
	if row == nil || row.Status != domain.MCPConfirmationExecuting || row.Fence != fence {
		return conflict("renew the claim on", key)
	}
	s.leases[row.ID] = s.now().Add(wholeSeconds(lease))
	return nil
}

// Complete records executed, failed, or unknown for a claim whose fence still holds.
func (s *Store) Complete(_ context.Context, key domain.MCPConfirmationKey, fence int64, status domain.MCPConfirmationStatus, result json.RawMessage, reason string) error {
	switch status {
	case domain.MCPConfirmationExecuted, domain.MCPConfirmationFailed, domain.MCPConfirmationUnknown:
	default:
		return fmt.Errorf("%w: %q is not an outcome", domain.ErrValidation, status)
	}
	if len(result) > 0 && (len(result) > confirmation.MaxDocumentBytes || !json.Valid(result)) {
		return fmt.Errorf("%w: result must be JSON of at most %d bytes", domain.ErrValidation, confirmation.MaxDocumentBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.find(key)
	if row == nil || row.Status != domain.MCPConfirmationExecuting || row.Fence != fence {
		return conflict("complete", key)
	}
	row.Status, row.Result, row.Error = status, cloneJSON(result), truncate(reason)
	delete(s.leases, row.ID)
	if status != domain.MCPConfirmationUnknown {
		row.CompletedAt = s.now()
	}
	return nil
}

// Reconcile records the verified outcome of an unknown confirmation.
func (s *Store) Reconcile(_ context.Context, key domain.MCPConfirmationKey, reconciler domain.PrincipalRef, outcome domain.MCPConfirmationStatus, note string) error {
	note = strings.TrimSpace(note)
	switch {
	case outcome != domain.MCPConfirmationExecuted && outcome != domain.MCPConfirmationFailed:
		return fmt.Errorf("%w: a reconciled outcome is executed or failed", domain.ErrValidation)
	case note == "" || utf8.RuneCountInString(note) > confirmation.MaxNoteRunes:
		return fmt.Errorf("%w: a reconciliation note is 1 to %d characters", domain.ErrValidation, confirmation.MaxNoteRunes)
	case reconciler.Kind == "" || strings.TrimSpace(reconciler.ID) == "":
		return fmt.Errorf("%w: reconciler is required", domain.ErrPrincipalUnknown)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.find(key)
	if row == nil || row.Status != domain.MCPConfirmationUnknown {
		return conflict("reconcile", key)
	}
	row.Status, row.Reconciler, row.ReconcileNote, row.CompletedAt = outcome, reconciler, note, s.now()
	return nil
}

// MarkLapsed turns a bounded batch of expired claims unknown.
func (s *Store) MarkLapsed(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var ids []int64
	for id, until := range s.leases {
		if !until.After(now) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b int64) int {
		return cmp.Or(s.leases[a].Compare(s.leases[b]), cmp.Compare(a, b))
	})
	limit = batch(limit)
	ids = ids[:min(limit, len(ids))]
	for _, id := range ids {
		s.rows[id].Status, s.rows[id].Error = domain.MCPConfirmationUnknown, "The run lost its claim before reporting an outcome; reconcile it."
		delete(s.leases, id)
	}
	return len(ids), nil
}

func validLease(lease time.Duration) error {
	if lease < time.Second || lease/time.Second > 1<<31-1 {
		return fmt.Errorf("%w: lease must be between a second and %d seconds", domain.ErrValidation, 1<<31-1)
	}
	return nil
}

func wholeSeconds(value time.Duration) time.Duration {
	return time.Duration(value/time.Second) * time.Second
}

func batch(limit int) int {
	switch {
	case limit <= 0:
		return confirmation.DefaultBatch
	case limit > confirmation.MaxBatch:
		return confirmation.MaxBatch
	}
	return limit
}

func truncate(note string) string {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) <= confirmation.MaxNoteRunes {
		return note
	}
	return string([]rune(note)[:confirmation.MaxNoteRunes])
}

var _ contract.MCPConfirmationStore = (*Store)(nil)

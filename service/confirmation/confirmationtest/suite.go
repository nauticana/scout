package confirmationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/confirmation"
)

// Harness is one store under test. Advance moves its store clock.
type Harness struct {
	Store      contract.MCPConfirmationStore
	Prepare    func(context.Context, domain.MCPConfirmationDraft) (domain.MCPConfirmation, bool, error)
	MarkLapsed func(context.Context, int) (int, error)
	Advance    func(time.Duration)
}

var (
	suiteMaker   = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "41"}
	suiteDecider = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
)

func suiteDraft(tool string, approvalRequired bool) domain.MCPConfirmationDraft {
	payload := []byte(`{"page":"home"}`)
	channel := domain.MCPConfirmationElicitation
	if approvalRequired {
		channel = domain.MCPConfirmationInbox
	}
	return domain.MCPConfirmationDraft{TenantID: 7, Maker: suiteMaker, ClientRef: "client-a", Tool: tool,
		Digest: confirmation.ActionDigest(tool, payload), Payload: payload, Preview: json.RawMessage(`{"action":"publish"}`),
		Channel: channel, ApprovalRequired: approvalRequired, TTL: 10 * time.Minute}
}

// RunStoreSuite checks the guarded transitions the executor relies on.
func RunStoreSuite(t *testing.T, newHarness func(*testing.T) Harness) {
	prepare := func(t *testing.T, h Harness, tool string, approvalRequired bool) domain.MCPConfirmationKey {
		t.Helper()
		stored, created, err := h.Prepare(t.Context(), suiteDraft(tool, approvalRequired))
		if err != nil || !created {
			t.Fatalf("prepare %s: created = %v, err = %v", tool, created, err)
		}
		return domain.MCPConfirmationKey{TenantID: stored.TenantID, ID: stored.ID}
	}
	approve := func(t *testing.T, h Harness, key domain.MCPConfirmationKey) {
		t.Helper()
		if err := h.Store.Decide(t.Context(), key, suiteDecider, true, "ok"); err != nil {
			t.Fatalf("decide: %v", err)
		}
	}

	t.Run("decides a pending confirmation once", func(t *testing.T) {
		h := newHarness(t)
		key := prepare(t, h, "publish", false)
		approve(t, h, key)
		if err := h.Store.Decide(t.Context(), key, suiteDecider, false, ""); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("second decision error = %v, want conflict", err)
		}
		stored, err := h.Store.Get(t.Context(), key)
		if err != nil || stored.Status != domain.MCPConfirmationApproved || stored.Decider != suiteDecider || stored.DecidedAt.IsZero() {
			t.Fatalf("stored = %+v, err = %v", stored, err)
		}
	})

	t.Run("refuses a decision past expiry or owned by maker-checker", func(t *testing.T) {
		h := newHarness(t)
		owned := prepare(t, h, "owned", true)
		if err := h.Store.Decide(t.Context(), owned, suiteDecider, true, ""); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("maker-checker decision error = %v, want conflict", err)
		}
		key := prepare(t, h, "late", false)
		h.Advance(11 * time.Minute)
		if err := h.Store.Decide(t.Context(), key, suiteDecider, true, ""); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("late decision error = %v, want conflict", err)
		}
		if stored, err := h.Store.Get(t.Context(), key); err != nil || !stored.Expired {
			t.Fatalf("stored = %+v, err = %v", stored, err)
		}
	})

	t.Run("reads only inside the tenant", func(t *testing.T) {
		h := newHarness(t)
		key := prepare(t, h, "publish", false)
		if _, err := h.Store.Get(t.Context(), domain.MCPConfirmationKey{TenantID: 8, ID: key.ID}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross-tenant read error = %v, want not found", err)
		}
	})

	t.Run("validates draft identity and documents", func(t *testing.T) {
		h := newHarness(t)
		invalid := suiteDraft("invalid", false)
		invalid.Digest = "not-a-digest"
		if _, _, err := h.Prepare(t.Context(), invalid); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid digest error = %v", err)
		}
		invalid.Digest = confirmation.ActionDigest(invalid.Tool, invalid.Payload)
		invalid.Preview = nil
		if _, _, err := h.Prepare(t.Context(), invalid); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("missing preview error = %v", err)
		}
	})

	t.Run("claims once and completes only under the current fence", func(t *testing.T) {
		h := newHarness(t)
		key := prepare(t, h, "publish", false)
		approve(t, h, key)
		if keys, err := h.Store.Approved(t.Context(), 10); err != nil || len(keys) != 1 || keys[0] != key {
			t.Fatalf("approved = %v, err = %v", keys, err)
		}
		fence, claimed, err := h.Store.Claim(t.Context(), key, time.Minute)
		if err != nil || !claimed {
			t.Fatalf("claim: claimed = %v, err = %v", claimed, err)
		}
		if _, again, err := h.Store.Claim(t.Context(), key, time.Minute); err != nil || again {
			t.Fatalf("second claim: claimed = %v, err = %v", again, err)
		}
		if err = h.Store.Renew(t.Context(), key, fence+1, time.Minute); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale renewal error = %v, want conflict", err)
		}
		if err = h.Store.Renew(t.Context(), key, fence, time.Minute); err != nil {
			t.Fatalf("renew: %v", err)
		}
		if err = h.Store.Complete(t.Context(), key, fence, domain.MCPConfirmationApproved, nil, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("non-outcome error = %v, want validation", err)
		}
		if err = h.Store.Complete(t.Context(), key, fence+1, domain.MCPConfirmationExecuted, nil, ""); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale completion error = %v, want conflict", err)
		}
		if err = h.Store.Complete(t.Context(), key, fence, domain.MCPConfirmationExecuted, json.RawMessage(`{"ok":true}`), ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		stored, err := h.Store.Get(t.Context(), key)
		if err != nil || stored.Status != domain.MCPConfirmationExecuted || string(stored.Result) != `{"ok":true}` || stored.Attempts != 1 || stored.CompletedAt.IsZero() {
			t.Fatalf("stored = %+v, err = %v", stored, err)
		}
		if keys, _ := h.Store.Approved(t.Context(), 10); len(keys) != 0 {
			t.Fatalf("executed confirmation still approved: %v", keys)
		}
	})

	t.Run("a lapsed claim becomes unknown and only reconciliation closes it", func(t *testing.T) {
		h := newHarness(t)
		key := prepare(t, h, "publish", false)
		approve(t, h, key)
		fence, _, err := h.Store.Claim(t.Context(), key, time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		h.Advance(2 * time.Second)
		if lapsed, err := h.MarkLapsed(t.Context(), 10); err != nil || lapsed != 1 {
			t.Fatalf("lapsed = %d, err = %v", lapsed, err)
		}
		if err = h.Store.Renew(t.Context(), key, fence, time.Minute); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("renewal after lapse error = %v, want conflict", err)
		}
		if _, claimed, _ := h.Store.Claim(t.Context(), key, time.Minute); claimed {
			t.Fatal("an unknown confirmation was claimed again")
		}
		if err = h.Store.Reconcile(t.Context(), key, suiteDecider, domain.MCPConfirmationUnknown, "checked"); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("unknown reconciliation error = %v, want validation", err)
		}
		if err = h.Store.Reconcile(t.Context(), key, suiteDecider, domain.MCPConfirmationExecuted, "checked the target"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if err = h.Store.Reconcile(t.Context(), key, suiteDecider, domain.MCPConfirmationFailed, "again"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("second reconciliation error = %v, want conflict", err)
		}
		if stored, err := h.Store.Get(t.Context(), key); err != nil || stored.Status != domain.MCPConfirmationExecuted || stored.Reconciler != suiteDecider {
			t.Fatalf("stored = %+v, err = %v", stored, err)
		}
	})

	t.Run("lapse processing honors its batch limit", func(t *testing.T) {
		h := newHarness(t)
		first := prepare(t, h, "first", false)
		second := prepare(t, h, "second", false)
		for _, key := range []domain.MCPConfirmationKey{first, second} {
			approve(t, h, key)
			if _, claimed, err := h.Store.Claim(t.Context(), key, time.Second); err != nil || !claimed {
				t.Fatalf("claim %v: claimed = %v, err = %v", key, claimed, err)
			}
		}
		h.Advance(2 * time.Second)
		if lapsed, err := h.MarkLapsed(t.Context(), 1); err != nil || lapsed != 1 {
			t.Fatalf("first batch = %d, err = %v", lapsed, err)
		}
		if lapsed, err := h.MarkLapsed(t.Context(), 1); err != nil || lapsed != 1 {
			t.Fatalf("second batch = %d, err = %v", lapsed, err)
		}
	})
}

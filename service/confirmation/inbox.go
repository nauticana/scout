package confirmation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/approval"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MakerChecker is the part of keel's approval service the inbox drives.
type MakerChecker interface {
	Decide(ctx context.Context, partnerID, id, checkerID int64, approved bool, note string) (*approval.Request, error)
	Withdraw(ctx context.Context, partnerID, id, makerID int64, note string) (*approval.Request, error)
}

var _ MakerChecker = (*approval.Service)(nil)

// Inbox lists and acts on confirmations across the tenants a person may act for. Every
// action is keyed by tenant and id, and a confirmation outside the person's view is not found.
type Inbox struct {
	Store      *TableStore
	Executor   *Executor
	Authorizer contract.MCPInboxAuthorizer
	// Approvals decides and withdraws the keel maker-checker requests of confirmations that require one.
	Approvals MakerChecker
}

func (in *Inbox) viewer(ctx context.Context, person domain.PrincipalRef) (contract.MCPInboxViewer, error) {
	if person.Kind == "" || strings.TrimSpace(person.ID) == "" {
		return nil, fmt.Errorf("%w: the inbox needs a person", domain.ErrPrincipalUnknown)
	}
	if in.Store == nil || in.Authorizer == nil {
		return nil, fmt.Errorf("%w: the confirmation inbox needs a store and an authorizer", domain.ErrNotReady)
	}
	viewer, err := in.Authorizer.Viewer(ctx, person)
	if err != nil {
		return nil, err
	}
	if viewer == nil {
		return nil, fmt.Errorf("%w: no inbox scope for the person", domain.ErrForbidden)
	}
	return viewer, nil
}

// visible reads one confirmation the person may see: in a viewed tenant or their own.
func (in *Inbox) visible(ctx context.Context, viewer contract.MCPInboxViewer, person domain.PrincipalRef, key domain.MCPConfirmationKey) (domain.MCPConfirmation, error) {
	stored, err := in.Store.Get(ctx, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if stored.Maker != person && !slices.Contains(viewer.Tenants(), stored.TenantID) {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	return stored, nil
}

// List returns the person's inbox: open confirmations and those created since, newest first,
// each with what the person may do now. A negative limit is refused; zero takes the default.
func (in *Inbox) List(ctx context.Context, person domain.PrincipalRef, since time.Time, limit int) ([]domain.MCPInboxEntry, error) {
	viewer, err := in.viewer(ctx, person)
	if err != nil {
		return nil, err
	}
	listed, err := in.Store.Inbox(ctx, viewer.Tenants(), person, since, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]domain.MCPInboxEntry, 0, len(listed))
	for _, stored := range listed {
		entry := domain.MCPInboxEntry{MCPConfirmation: stored}
		switch {
		case stored.Status == domain.MCPConfirmationPending && !stored.Expired:
			entry.CanWithdraw = stored.Maker == person
			entry.CanDecide = (!stored.ApprovalRequired || (stored.ApprovalID > 0 && humanID(person) > 0)) &&
				viewer.Decider(ctx, stored) == nil
		case stored.Status == domain.MCPConfirmationUnknown:
			entry.CanReconcile = viewer.Reconciler(ctx, stored) == nil
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Decide records the person's decision. A confirmation that requires maker-checker approval is
// decided through keel with the person as checker; any other through Executor.Decide.
func (in *Inbox) Decide(ctx context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey, approve bool, note string) (domain.MCPConfirmation, error) {
	viewer, err := in.viewer(ctx, person)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	stored, err := in.visible(ctx, viewer, person, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if err = viewer.Decider(ctx, stored); err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: %w", domain.ErrForbidden, err)
	}
	if !stored.ApprovalRequired {
		if in.Executor == nil {
			return domain.MCPConfirmation{}, fmt.Errorf("%w: direct decisions need an executor", domain.ErrNotReady)
		}
		return in.Executor.Decide(ctx, key, person, approve, note)
	}
	if in.Approvals == nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: maker-checker decisions need the approval service", domain.ErrNotReady)
	}
	checkerID := humanID(person)
	switch {
	case checkerID <= 0:
		return domain.MCPConfirmation{}, fmt.Errorf("%w: a maker-checker decision needs a human with a numeric id", domain.ErrValidation)
	case stored.ApprovalID <= 0:
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d has no approval request yet", domain.ErrConflict, key.ID)
	}
	if _, err = in.Approvals.Decide(ctx, key.TenantID, stored.ApprovalID, checkerID, approve, keelNote(note)); err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("decide confirmation %d: %w", key.ID, keelError(err))
	}
	return in.Store.Get(ctx, key)
}

// Withdraw closes the person's own pending confirmation and its keel request. A keel request
// already closed is tolerated; one that cannot be withdrawn is reported and left to expiry.
func (in *Inbox) Withdraw(ctx context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey) error {
	if _, err := in.viewer(ctx, person); err != nil {
		return err
	}
	stored, err := in.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	if stored.Maker != person {
		return fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	makerID := humanID(person)
	if stored.ApprovalRequired && (in.Approvals == nil || makerID <= 0) {
		return fmt.Errorf("%w: withdrawing a maker-checker confirmation needs the approval service and a human maker with a numeric id", domain.ErrNotReady)
	}
	approvalID, err := in.Store.Withdraw(ctx, key, person)
	if err != nil || approvalID == 0 {
		return err
	}
	_, err = in.Approvals.Withdraw(context.WithoutCancel(ctx), key.TenantID, approvalID, makerID, "")
	if err != nil && !errors.Is(err, approval.ErrInvalidState) && !errors.Is(err, approval.ErrExpired) {
		return fmt.Errorf("confirmation %d is withdrawn but approval request %d stays open until expiry: %w", key.ID, approvalID, keelError(err))
	}
	return nil
}

// Reconcile records what the person verified about an unknown outcome.
func (in *Inbox) Reconcile(ctx context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey, outcome domain.MCPConfirmationStatus, note string) (domain.MCPConfirmation, error) {
	viewer, err := in.viewer(ctx, person)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if in.Executor == nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: reconciliation needs an executor", domain.ErrNotReady)
	}
	stored, err := in.visible(ctx, viewer, person, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if err = viewer.Reconciler(ctx, stored); err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: %w", domain.ErrForbidden, err)
	}
	return in.Executor.Reconcile(ctx, key, person, outcome, note)
}

// OnDecided is approval.Service.OnDecided: it applies decisions on ApprovalObjectType and
// ignores other object types, so a product can chain its own.
func (in *Inbox) OnDecided(ctx context.Context, tx keelport.TxQueryService, request *approval.Request) error {
	if request == nil || request.ObjectType != ApprovalObjectType {
		return nil
	}
	if in.Store == nil {
		return fmt.Errorf("%w: confirmation approvals need a store", domain.ErrNotReady)
	}
	return in.Store.DecideApprovalTx(ctx, tx, request)
}

// humanID is a human principal's decimal user id, or 0.
func humanID(person domain.PrincipalRef) int64 {
	if person.Kind != domain.PrincipalHuman {
		return 0
	}
	id, err := strconv.ParseInt(person.ID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != person.ID {
		return 0
	}
	return id
}

// keelMaxNoteBytes is keel's approval note limit.
const keelMaxNoteBytes = 500

// keelNote truncates a note as the store does and to keel's byte limit.
func keelNote(note string) string {
	note = truncate(note)
	for len(note) > keelMaxNoteBytes {
		_, size := utf8.DecodeLastRuneInString(note)
		note = note[:len(note)-size]
	}
	return note
}

// keelError maps keel's maker-checker errors to Scout sentinels.
func keelError(err error) error {
	switch {
	case errors.Is(err, approval.ErrNotFound):
		return fmt.Errorf("%w: %w", domain.ErrNotFound, err)
	case errors.Is(err, approval.ErrInvalidState), errors.Is(err, approval.ErrExpired):
		return fmt.Errorf("%w: %w", domain.ErrConflict, err)
	case errors.Is(err, approval.ErrSameActor), errors.Is(err, approval.ErrNotMaker):
		return fmt.Errorf("%w: %w", domain.ErrForbidden, err)
	}
	return err
}

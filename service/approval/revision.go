package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultMaxRevisedArgumentBytes bounds the revised arguments of one edited verdict.
const DefaultMaxRevisedArgumentBytes = 64 << 10

// ArgumentsDigest is the SHA-256 of revised arguments exactly as archived.
func ArgumentsDigest(arguments []byte) string {
	sum := sha256.Sum256(arguments)
	return hex.EncodeToString(sum[:])
}

// Revision returns the arguments a decider substituted for call when its proposal was edited.
func (g *Gate) Revision(ctx context.Context, call domain.ToolCall) (domain.CallRevision, bool, error) {
	if g.Store == nil {
		return domain.CallRevision{}, false, fmt.Errorf("approval gate: store is required")
	}
	request, key, edited, err := g.edited(ctx, call)
	if err != nil || !edited {
		return domain.CallRevision{}, false, err
	}
	if request.ProposedDigest != ProposalDigest(call) {
		return domain.CallRevision{}, false, fmt.Errorf("%w: the edited proposal of step %d is not this call", domain.ErrConflict, key.ExecutionStepID)
	}
	if g.Archive == nil {
		return domain.CallRevision{}, false, fmt.Errorf("%w: an edited tool call needs the evidence archive of its revised arguments", domain.ErrNotReady)
	}
	edit, err := g.Store.Revision(ctx, key)
	if err != nil {
		return domain.CallRevision{}, false, err
	}
	arguments, err := g.Archive.Hydrate(ctx, edit.Arguments)
	if err != nil {
		return domain.CallRevision{}, false, fmt.Errorf("load revised arguments of step %d: %w", key.ExecutionStepID, err)
	}
	if ArgumentsDigest(arguments) != edit.Arguments.Digest {
		return domain.CallRevision{}, false, fmt.Errorf("%w: revised arguments of step %d no longer match their digest", domain.ErrConflict, key.ExecutionStepID)
	}
	return domain.CallRevision{RevisesProposal: key.ExecutionStepID, Arguments: arguments, ArgumentsDigest: edit.Arguments.Digest}, true, nil
}

// edited walks the call's requests past those approved and reports the first one edited.
func (g *Gate) edited(ctx context.Context, call domain.ToolCall) (domain.ApprovalRequest, domain.ApprovalKey, bool, error) {
	maxAsks, err := g.maxAsks()
	if err != nil {
		return domain.ApprovalRequest{}, domain.ApprovalKey{}, false, err
	}
	for index := 0; index < maxAsks; index++ {
		key := domain.ApprovalKey{TenantID: call.TenantContext.TenantID, RequestID: call.RequestID, ExecutionStepID: stepIDAt(call, index)}
		request, err := g.Store.Get(ctx, key)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ApprovalRequest{}, key, false, nil
		}
		if err != nil {
			return domain.ApprovalRequest{}, key, false, err
		}
		switch request.Status {
		case domain.ApprovalStatusEdited:
			return request, key, true, nil
		case domain.ApprovalStatusApproved:
		default:
			return domain.ApprovalRequest{}, key, false, nil
		}
	}
	return domain.ApprovalRequest{}, domain.ApprovalKey{}, false, nil
}

// edit returns the edit call claims to carry, refusing a call that names a proposal
// nobody edited, another tool, or arguments other than the archived revision.
func (g *Gate) edit(ctx context.Context, call domain.ToolCall) (domain.ProposalRevision, error) {
	key := domain.ApprovalKey{TenantID: call.TenantContext.TenantID, RequestID: call.RequestID, ExecutionStepID: call.RevisesProposal}
	original, err := g.Store.Get(ctx, key)
	if err != nil {
		return domain.ProposalRevision{}, fmt.Errorf("load revised proposal: %w", err)
	}
	switch {
	case original.Status != domain.ApprovalStatusEdited:
		return domain.ProposalRevision{}, fmt.Errorf("%w: step %d was not edited", domain.ErrConflict, key.ExecutionStepID)
	case original.Resource != call.ToolID:
		return domain.ProposalRevision{}, fmt.Errorf("%w: an edit of %q cannot call %q", domain.ErrConflict, original.Resource, call.ToolID)
	}
	edit, err := g.Store.Revision(ctx, key)
	if err != nil {
		return domain.ProposalRevision{}, err
	}
	if ArgumentsDigest(call.Arguments) != edit.Arguments.Digest {
		return domain.ProposalRevision{}, fmt.Errorf("%w: the call does not carry the revised arguments of step %d", domain.ErrConflict, key.ExecutionStepID)
	}
	return edit, nil
}

// approveAsEditor records the editor's verdict on the revision it proposed when the editor
// may decide that revision; otherwise the revision stays open for its own approver.
func (g *Gate) approveAsEditor(ctx context.Context, revised domain.ApprovalRequest, edit domain.ProposalRevision) (domain.ApprovalRequest, error) {
	if g.Authorizer == nil {
		return revised, nil
	}
	err := g.Authorizer.AuthorizeApproval(ctx, revised, edit.Editor)
	if errors.Is(err, domain.ErrForbidden) {
		return revised, nil
	}
	if err != nil {
		return revised, err
	}
	key := domain.ApprovalKey{TenantID: revised.TenantID, RequestID: revised.RequestID, ExecutionStepID: revised.ExecutionStepID}
	verdict := domain.ApprovalVerdict{
		RequestKey: key, Status: domain.ApprovalStatusApproved, Decider: edit.Editor, Authority: edit.Authority,
		ProposedDigest: revised.ProposedDigest, Reason: edit.Reason, DecidedAt: g.now(),
		Assignment: fenceOf(revised),
	}
	resolved, err := g.Store.Resolve(ctx, verdict)
	if errors.Is(err, domain.ErrConflict) {
		// Another verdict landed first; it is the one that stands.
		return g.Store.Get(ctx, key)
	}
	if err != nil {
		return revised, err
	}
	return resolved, recordVerdict(ctx, g.Audit, resolved, verdict, g.now())
}

// archiveRevision validates an edited verdict's revised arguments and archives them where
// evidence lives, so the verdict row carries only their reference and digest.
func (r *Resumer) archiveRevision(ctx context.Context, request domain.ApprovalRequest, verdict domain.ApprovalVerdict) (domain.ApprovalVerdict, error) {
	if !strings.HasPrefix(request.Action, "tool:") {
		return verdict, fmt.Errorf("%w: only a proposed tool call can be edited", domain.ErrValidation)
	}
	if r.Archive == nil {
		return verdict, fmt.Errorf("%w: editing a proposal needs an evidence archive", domain.ErrNotReady)
	}
	limit := r.MaxRevisedArgumentBytes
	switch {
	case limit < 0:
		return verdict, fmt.Errorf("approval resumer: max revised argument bytes cannot be negative")
	case limit == 0:
		limit = DefaultMaxRevisedArgumentBytes
	}
	arguments, err := revisedArguments(verdict.RevisedArguments, limit)
	if err != nil {
		return verdict, err
	}
	if verdict.ProposedDigest != request.ProposedDigest {
		return verdict, fmt.Errorf("%w: request %q step %d is not awaiting this proposal", domain.ErrConflict, request.RequestID, request.ExecutionStepID)
	}
	switch request.Status {
	case domain.ApprovalStatusPending, domain.ApprovalStatusEscalated:
	case domain.ApprovalStatusEdited:
		// An exact retry reuses the archived revision; nothing is written for a closed request.
		edit, err := r.Store.Revision(ctx, verdict.RequestKey)
		if err != nil {
			return verdict, err
		}
		if edit.Arguments.Digest != ArgumentsDigest(arguments) {
			return verdict, fmt.Errorf("%w: request %q step %d was already edited", domain.ErrConflict, request.RequestID, request.ExecutionStepID)
		}
		verdict.RevisedArguments, verdict.Revision = arguments, edit.Arguments
		return verdict, nil
	default:
		return verdict, fmt.Errorf("%w: request %q step %d is already %s", domain.ErrConflict, request.RequestID, request.ExecutionStepID, request.Status)
	}
	ref, err := r.Archive.Dehydrate(ctx, revisionName(verdict.RequestKey), arguments)
	if err != nil {
		return verdict, fmt.Errorf("archive revised arguments: %w", err)
	}
	if ref.URI == "" || ref.Digest != ArgumentsDigest(arguments) {
		return verdict, fmt.Errorf("archive revised arguments: the archive returned a reference that does not digest them")
	}
	verdict.RevisedArguments, verdict.Revision = arguments, ref
	return verdict, nil
}

// revisedArguments accepts exactly one JSON object within limit and compacts it, keeping
// every number as written.
func revisedArguments(raw []byte, limit int) ([]byte, error) {
	switch {
	case len(bytes.TrimSpace(raw)) == 0:
		return nil, fmt.Errorf("%w: an edited verdict needs revised arguments", domain.ErrValidation)
	case len(raw) > limit:
		return nil, fmt.Errorf("%w: revised arguments exceed %d bytes", domain.ErrValidation, limit)
	case !json.Valid(raw):
		return nil, fmt.Errorf("%w: revised arguments are not one JSON value", domain.ErrValidation)
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return nil, fmt.Errorf("%w: revised arguments: %w", domain.ErrValidation, err)
	}
	if compacted.Bytes()[0] != '{' {
		return nil, fmt.Errorf("%w: revised arguments must be a JSON object", domain.ErrValidation)
	}
	return compacted.Bytes(), nil
}

func revisionName(key domain.ApprovalKey) string {
	return fmt.Sprintf("approval/%d/%s/%d/revision", key.TenantID, url.PathEscape(key.RequestID), key.ExecutionStepID)
}

var _ contract.ProposalRevisions = (*Gate)(nil)

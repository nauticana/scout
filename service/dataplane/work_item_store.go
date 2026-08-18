package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qWorkAssign    = "scout_work_item_assign"
	qWorkGet       = "scout_work_item_get"
	qWorkByRequest = "scout_work_item_by_request"
	qWorkPending   = "scout_work_item_pending"
	qWorkComplete  = "scout_work_item_complete"
	qWorkAncestors = "scout_work_item_ancestors"

	workColumns = `id, assignee_kind, assignee_id, requester_kind, requester_id, grant_id,
       parent_work_item_id, delegation_depth, scope_id, task_kind, input_uri, input_digest,
       request_id, status_code, budget_minor_units, currency_code, created_at, completed_at`

	// maxWorkItemDepth bounds the ancestor walk; a chain deeper than this is a
	// configuration error, not a legitimate delegation.
	maxWorkItemDepth = 32
)

var workItemQueries = map[string]string{
	qWorkAssign: `
INSERT INTO agent_work_item (id, tenant_id, assignee_kind, assignee_id, requester_kind, requester_id, grant_id,
                       parent_work_item_id, delegation_depth, scope_id, task_kind, input_uri, input_digest,
                       request_id, status_code, budget_minor_units, currency_code)
VALUES (nextval('agent_work_item_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)
ON CONFLICT (tenant_id, request_id) DO NOTHING
RETURNING id`,
	qWorkGet: `
SELECT ` + workColumns + `
  FROM agent_work_item WHERE tenant_id = ? AND id = ?`,
	qWorkByRequest: `
SELECT ` + workColumns + `
  FROM agent_work_item WHERE tenant_id = ? AND request_id = ?`,
	qWorkPending: `
SELECT ` + workColumns + `
  FROM agent_work_item
 WHERE tenant_id = ? AND assignee_kind = ? AND assignee_id = ?
   AND status_code IN ('queued', 'running', 'streaming', 'suspended')
 ORDER BY created_at, id
 LIMIT ?`,
	qWorkComplete: `
UPDATE agent_work_item
   SET status_code = ?, completed_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND id = ? AND completed_at IS NULL
RETURNING id`,
	qWorkAncestors: `
WITH RECURSIVE chain AS (
    SELECT ` + workColumns + `
      FROM agent_work_item WHERE tenant_id = ? AND id = ?
    UNION ALL
    SELECT parent.` + `id, parent.assignee_kind, parent.assignee_id, parent.requester_kind, parent.requester_id,
           parent.grant_id, parent.parent_work_item_id, parent.delegation_depth, parent.scope_id, parent.task_kind,
           parent.input_uri, parent.input_digest, parent.request_id, parent.status_code,
           parent.budget_minor_units, parent.currency_code, parent.created_at, parent.completed_at
      FROM agent_work_item parent
      JOIN chain child ON child.parent_work_item_id = parent.id
)
SELECT * FROM chain LIMIT ?`,
}

// TableWorkItemStore addresses work to a principal rather than to a conversation,
// so an agent can be assigned work it did not start.
type TableWorkItemStore struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

func (s *TableWorkItemStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("work item store: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, workItemQueries) })
	if s.qs == nil {
		return fmt.Errorf("work item store: query service is required")
	}
	return nil
}

// Assign records a new item; a repeated request id returns the existing one, so
// a redelivered delegation does not fan out twice.
func (s *TableWorkItemStore) Assign(ctx context.Context, item domain.WorkItem) (domain.WorkItem, error) {
	if err := s.init(ctx); err != nil {
		return domain.WorkItem{}, err
	}
	switch {
	case item.TenantID <= 0 || item.Assignee.ID == "" || item.Requester.ID == "":
		return domain.WorkItem{}, fmt.Errorf("%w: a work item needs tenant, assignee, and requester", domain.ErrValidation)
	case strings.TrimSpace(item.RequestID) == "":
		return domain.WorkItem{}, fmt.Errorf("%w: a work item needs a request id to stay idempotent", domain.ErrValidation)
	case len(item.Input.Digest) != 64:
		return domain.WorkItem{}, fmt.Errorf("%w: a work item needs a digest-verified input reference", domain.ErrValidation)
	case item.Depth < 0 || item.Depth > maxWorkItemDepth:
		return domain.WorkItem{}, fmt.Errorf("%w: work item depth %d is out of range", domain.ErrDelegationDepth, item.Depth)
	}
	ctx = context.WithoutCancel(ctx)
	inserted, err := s.qs.Query(ctx, qWorkAssign, item.TenantID,
		string(item.Assignee.Kind), item.Assignee.ID, string(item.Requester.Kind), item.Requester.ID,
		nullableString(item.GrantID), nullableID(item.ParentID), item.Depth, nullableString(item.ScopeID),
		item.TaskKind, item.Input.URI, item.Input.Digest, item.RequestID,
		nullableAmount(item.BudgetMinorUnits), nullableString(item.Currency))
	if err != nil {
		return domain.WorkItem{}, fmt.Errorf("assign work item: %w", err)
	}
	if len(inserted.Rows) > 0 {
		item.ID = common.AsInt64(inserted.Rows[0][0])
		item.Status = "queued"
		return item, nil
	}
	existing, err := s.qs.Query(ctx, qWorkByRequest, item.TenantID, item.RequestID)
	if err != nil {
		return domain.WorkItem{}, fmt.Errorf("assign work item: %w", err)
	}
	if len(existing.Rows) == 0 {
		return domain.WorkItem{}, fmt.Errorf("work item %q disappeared after insert", item.RequestID)
	}
	return scanWorkItem(item.TenantID, existing.Rows[0]), nil
}

// Get returns one item.
func (s *TableWorkItemStore) Get(ctx context.Context, tenantID, id int64) (domain.WorkItem, error) {
	if err := s.init(ctx); err != nil {
		return domain.WorkItem{}, err
	}
	result, err := s.qs.Query(ctx, qWorkGet, tenantID, id)
	if err != nil {
		return domain.WorkItem{}, fmt.Errorf("get work item: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.WorkItem{}, fmt.Errorf("%w: work item %d", domain.ErrNotFound, id)
	}
	return scanWorkItem(tenantID, result.Rows[0]), nil
}

// Pending lists open items for one assignee, oldest first.
func (s *TableWorkItemStore) Pending(ctx context.Context, tenantID int64, assignee domain.PrincipalRef, limit int) ([]domain.WorkItem, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	result, err := s.qs.Query(ctx, qWorkPending, tenantID, string(assignee.Kind), assignee.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	items := make([]domain.WorkItem, 0, len(result.Rows))
	for _, row := range result.Rows {
		items = append(items, scanWorkItem(tenantID, row))
	}
	return items, nil
}

// Complete records a terminal status once.
func (s *TableWorkItemStore) Complete(ctx context.Context, tenantID, id int64, status string) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if !isTerminalTurnStatus(status) {
		return fmt.Errorf("%w: %q is not a terminal work-item status", domain.ErrValidation, status)
	}
	ctx = context.WithoutCancel(ctx)
	updated, err := s.qs.Query(ctx, qWorkComplete, status, tenantID, id)
	if err != nil {
		return fmt.Errorf("complete work item: %w", err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: work item %d is already terminal", domain.ErrConflict, id)
	}
	return nil
}

// Ancestors returns the item and its chain to the root, nearest first.
func (s *TableWorkItemStore) Ancestors(ctx context.Context, tenantID, id int64) ([]domain.WorkItem, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qWorkAncestors, tenantID, id, maxWorkItemDepth)
	if err != nil {
		return nil, fmt.Errorf("walk work item ancestors: %w", err)
	}
	items := make([]domain.WorkItem, 0, len(result.Rows))
	for _, row := range result.Rows {
		items = append(items, scanWorkItem(tenantID, row))
	}
	return items, nil
}

func scanWorkItem(tenantID int64, row []any) domain.WorkItem {
	completed, _ := common.AsTimeOK(row[17])
	return domain.WorkItem{
		ID: common.AsInt64(row[0]), TenantID: tenantID,
		Assignee:  domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[1])), ID: common.AsString(row[2])},
		Requester: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[3])), ID: common.AsString(row[4])},
		GrantID:   common.AsString(row[5]), ParentID: common.AsInt64(row[6]), Depth: int(common.AsInt64(row[7])),
		ScopeID: common.AsString(row[8]), TaskKind: common.AsString(row[9]),
		Input:     domain.ObjectRef{URI: common.AsString(row[10]), Digest: common.AsString(row[11])},
		RequestID: common.AsString(row[12]), Status: common.AsString(row[13]),
		BudgetMinorUnits: common.AsInt64(row[14]), Currency: common.AsString(row[15]),
		CreatedAt: common.AsTime(row[16]), CompletedAt: completed,
	}
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableID(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableAmount(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

var _ contract.WorkItemStore = (*TableWorkItemStore)(nil)

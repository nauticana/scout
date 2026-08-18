package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qDecisionInsert = "scout_decision_insert"
	qDecisionPage   = "scout_decision_page"

	// DefaultDecisionPageSize bounds an unbounded audit query.
	DefaultDecisionPageSize = 100
	// MaxDecisionPageSize caps what a caller may ask for in one page.
	MaxDecisionPageSize = 1000
)

// The read query filters on tenant first and takes every other predicate as an
// optional NULL-guarded comparison, so one statement serves every timeline view.
var decisionQueries = map[string]string{
	qDecisionInsert: `
INSERT INTO audit_event
       (id, tenant_id, category, principal_kind, principal_id, grant_id, grantor_kind, grantor_id,
        scope_id, performed_action, resource_ref, release_version, policy_id, policy_version, outcome_code,
        obligations, reason, request_id, conversation_id, payload_uri, payload_digest, occurred_at)
VALUES (nextval('audit_event_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qDecisionPage: `
SELECT id, category, principal_kind, principal_id, grant_id, grantor_kind, grantor_id,
       scope_id, performed_action, resource_ref, release_version, policy_id, policy_version, outcome_code,
       obligations, reason, request_id, conversation_id, payload_uri, payload_digest, occurred_at
  FROM audit_event
 WHERE ((? > 0 AND tenant_id = ?) OR (? = 0 AND tenant_id IS NULL))
   AND (? = '' OR category = ?)
   AND (? = '' OR principal_kind = ?)
   AND (? = '' OR principal_id = ?)
   AND (? = '' OR resource = ?)
   AND (? = '' OR request_id = ?)
   AND (? = '' OR conversation_id = ?)
   AND (? = '' OR outcome_code = ?)
   AND (?::timestamp IS NULL OR occurred_at >= ?)
   AND (?::timestamp IS NULL OR occurred_at < ?)
   AND (? = 0 OR id < ?)
 ORDER BY id DESC
 LIMIT ?`,
}

// EvidenceStore moves a redacted decision payload to object storage; the row
// keeps only the returned reference.
type EvidenceStore interface {
	Dehydrate(ctx context.Context, name string, payload []byte) (domain.ObjectRef, error)
}

// TableAuditSink is the durable evidence trail over audit_event. It is both the
// write and the read side: evidence with no way to read it answers nothing.
type TableAuditSink struct {
	DB keelport.DatabaseRepository
	// Evidence is optional; without it a record keeps its typed columns and drops
	// the payload, because a decision must be recorded even when storage is not configured.
	Evidence EvidenceStore

	once sync.Once
	qs   keelport.QueryService
}

func (sink *TableAuditSink) init(ctx context.Context) error {
	if sink.DB == nil {
		return fmt.Errorf("audit sink: database is required")
	}
	sink.once.Do(func() { sink.qs = sink.DB.GetQueryService(ctx, decisionQueries) })
	if sink.qs == nil {
		return fmt.Errorf("audit sink: query service is required")
	}
	return nil
}

// Record writes one decision. Writes never inherit the caller's cancellation:
// evidence for work that already happened must survive a client disconnect.
func (sink *TableAuditSink) Record(ctx context.Context, decision domain.DecisionRecord) error {
	if err := sink.init(ctx); err != nil {
		return err
	}
	if decision.TenantID < 0 || strings.TrimSpace(decision.Category) == "" ||
		strings.TrimSpace(decision.Action) == "" || decision.Outcome == "" {
		return fmt.Errorf("%w: decision needs tenant, category, action, and outcome", domain.ErrValidation)
	}
	if decision.Principal.Kind == "" || strings.TrimSpace(decision.Principal.ID) == "" {
		return fmt.Errorf("%w: decision needs the acting principal", domain.ErrPrincipalUnknown)
	}
	if decision.TenantID == 0 && strings.TrimSpace(decision.ScopeID) != "" {
		return fmt.Errorf("%w: a platform-wide decision cannot name a tenant scope", domain.ErrValidation)
	}
	occurred := decision.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	if len(decision.Payload) > 0 && sink.Evidence != nil {
		ref, err := sink.Evidence.Dehydrate(context.WithoutCancel(ctx), evidenceName(decision), decision.Payload)
		if err != nil {
			return fmt.Errorf("store decision evidence: %w", err)
		}
		decision.Evidence = ref
	}
	obligations := make([]string, 0, len(decision.Obligations))
	for _, obligation := range decision.Obligations {
		obligations = append(obligations, string(obligation))
	}
	var tenantID any
	if decision.TenantID > 0 {
		tenantID = decision.TenantID
	}
	_, err := sink.qs.Query(context.WithoutCancel(ctx), qDecisionInsert,
		tenantID, decision.Category, string(decision.Principal.Kind), decision.Principal.ID,
		nullable(decision.Authority.GrantID), nullable(string(decision.Authority.Grantor.Kind)), nullable(decision.Authority.Grantor.ID),
		nullable(decision.ScopeID), decision.Action, nullable(decision.Resource), nullable(decision.ReleaseVersion),
		nullable(decision.PolicyID), nullable(decision.PolicyVersion), string(decision.Outcome),
		nullable(strings.Join(obligations, ",")), nullable(decision.Reason),
		nullable(decision.RequestID), nullable(decision.ConversationID),
		nullable(decision.Evidence.URI), nullable(decision.Evidence.Digest), occurred)
	if err != nil {
		return fmt.Errorf("record decision: %w", err)
	}
	return nil
}

// Decisions returns one page, newest first, always inside a single tenant.
func (sink *TableAuditSink) Decisions(ctx context.Context, query domain.DecisionQuery) (domain.DecisionPage, error) {
	if err := sink.init(ctx); err != nil {
		return domain.DecisionPage{}, err
	}
	if query.TenantID < 0 {
		return domain.DecisionPage{}, fmt.Errorf("%w: audit tenant id cannot be negative", domain.ErrValidation)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultDecisionPageSize
	}
	if limit > MaxDecisionPageSize {
		limit = MaxDecisionPageSize
	}
	var kind, id string
	if query.Principal != nil {
		kind, id = string(query.Principal.Kind), query.Principal.ID
	}
	result, err := sink.qs.Query(ctx, qDecisionPage, query.TenantID, query.TenantID, query.TenantID,
		query.Category, query.Category, kind, kind, id, id,
		query.Resource, query.Resource, query.RequestID, query.RequestID,
		query.ConversationID, query.ConversationID, string(query.Outcome), string(query.Outcome),
		nullableTime(query.Since), query.Since, nullableTime(query.Until), query.Until,
		query.Before, query.Before, limit)
	if err != nil {
		return domain.DecisionPage{}, fmt.Errorf("query decisions: %w", err)
	}
	page := domain.DecisionPage{Records: make([]domain.DecisionRecord, 0, len(result.Rows))}
	for _, row := range result.Rows {
		record := domain.DecisionRecord{
			TenantID: query.TenantID, Category: common.AsString(row[1]),
			Principal: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[2])), ID: common.AsString(row[3])},
			Authority: domain.AuthorityRef{
				GrantID: common.AsString(row[4]),
				Grantor: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[5])), ID: common.AsString(row[6])},
			},
			ScopeID: common.AsString(row[7]), Action: common.AsString(row[8]), Resource: common.AsString(row[9]),
			ReleaseVersion: common.AsString(row[10]), PolicyID: common.AsString(row[11]), PolicyVersion: common.AsString(row[12]),
			Outcome: domain.DecisionOutcome(common.AsString(row[13])), Reason: common.AsString(row[15]),
			RequestID: common.AsString(row[16]), ConversationID: common.AsString(row[17]),
			Evidence:   domain.ObjectRef{URI: common.AsString(row[18]), Digest: common.AsString(row[19])},
			OccurredAt: common.AsTime(row[20]),
		}
		for _, obligation := range strings.Split(common.AsString(row[14]), ",") {
			if obligation != "" {
				record.Obligations = append(record.Obligations, domain.ObligationKind(obligation))
			}
		}
		record.Authority.Subject = record.Principal
		page.Records = append(page.Records, record)
		page.NextBefore = common.AsInt64(row[0])
	}
	if len(page.Records) < limit {
		page.NextBefore = 0
	}
	return page, nil
}

func evidenceName(decision domain.DecisionRecord) string {
	sum := sha256.Sum256(decision.Payload)
	tenant := fmt.Sprintf("%d", decision.TenantID)
	if decision.TenantID == 0 {
		tenant = "platform"
	}
	return fmt.Sprintf("decision/%s/%s/%s", tenant, decision.Category, hex.EncodeToString(sum[:]))
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

var (
	_ contract.AuditSink  = (*TableAuditSink)(nil)
	_ contract.AuditQuery = (*TableAuditSink)(nil)
)

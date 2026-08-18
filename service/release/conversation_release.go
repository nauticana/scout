package release

import (
	"context"
	"encoding/json"
	"errors"
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
	qConversationReleaseGet     = "scout_release_conversation_get"
	qConversationReleasePut     = "scout_release_conversation_put"
	qConversationReleaseMigrate = "scout_release_conversation_migrate"
	qTenantRingOrder            = "scout_release_tenant_ring_order"
	qLiveReleaseCandidates      = "scout_release_live_candidates"
)

var conversationReleaseQueries = map[string]string{
	qConversationReleaseGet: `
SELECT c.agent_version, r.platform_version, r.resolved_at
  FROM conversation_release r
  JOIN agent_conversation c ON c.tenant_id = r.tenant_id AND c.conversation_id = r.conversation_id
 WHERE r.tenant_id = ? AND r.conversation_id = ?`,
	// Inserts only against an existing conversation on the same agent version,
	// so the two identities can never disagree.
	qConversationReleasePut: `
INSERT INTO conversation_release (tenant_id, conversation_id, platform_version, resolved_at)
SELECT ?, ?, ?, ?
  FROM agent_conversation c
 WHERE c.tenant_id = ? AND c.conversation_id = ? AND c.agent_version = ?
   AND NOT EXISTS (SELECT 1 FROM conversation_release WHERE tenant_id = ? AND conversation_id = ?)
RETURNING conversation_id`,
	qConversationReleaseMigrate: `
UPDATE conversation_release
   SET platform_version = ?, resolved_at = ?
 WHERE tenant_id = ? AND conversation_id = ?
RETURNING conversation_id`,
	qTenantRingOrder: `
SELECT ring.rollout_order
  FROM tenant_ring_member m
  JOIN tenant_ring ring ON ring.ring_code = m.ring_code
 WHERE m.tenant_id = ?`,
	// Paused releases keep their sessions but take no new ones; shadow never takes user sessions.
	qLiveReleaseCandidates: `
SELECT s.platform_version, s.stage_code, s.traffic_percentage, COALESCE(ring.rollout_order, 0)
  FROM platform_rollout_state s
  JOIN rollout_stage stage ON stage.code = s.stage_code
  LEFT JOIN tenant_ring ring ON ring.ring_code = s.ring_code
 WHERE stage.is_live = TRUE AND s.paused = FALSE AND s.stage_code <> 'shadow'
 ORDER BY stage.stage_order DESC, s.stage_started_at DESC, s.platform_version`,
}

// TableConversationReleaseStore is the keel-backed ConversationReleaseStore;
// the agent version is read from agent_conversation, the platform release from conversation_release.
type TableConversationReleaseStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ConversationReleaseStore = (*TableConversationReleaseStore)(nil)

func (store *TableConversationReleaseStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("conversation release store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, conversationReleaseQueries) })
	if store.qs == nil {
		return fmt.Errorf("conversation release store: query service is required")
	}
	return nil
}

func (store *TableConversationReleaseStore) Get(ctx context.Context, tenantID int64, conversationID string) (domain.ConversationRelease, error) {
	if err := store.init(ctx); err != nil {
		return domain.ConversationRelease{}, err
	}
	result, err := store.qs.Query(ctx, qConversationReleaseGet, tenantID, conversationID)
	if err != nil {
		return domain.ConversationRelease{}, fmt.Errorf("get conversation release: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.ConversationRelease{}, fmt.Errorf("%w: release for conversation %s", domain.ErrNotFound, conversationID)
	}
	row := result.Rows[0]
	return domain.ConversationRelease{
		TenantID: tenantID, ConversationID: conversationID, AgentVersion: common.AsString(row[0]),
		PlatformVersion: common.AsString(row[1]), ResolvedAt: common.AsTime(row[2]),
	}, nil
}

func (store *TableConversationReleaseStore) Put(ctx context.Context, release domain.ConversationRelease) error {
	if release.TenantID <= 0 || strings.TrimSpace(release.ConversationID) == "" || strings.TrimSpace(release.AgentVersion) == "" || strings.TrimSpace(release.PlatformVersion) == "" {
		return fmt.Errorf("%w: conversation release needs tenant, conversation, agent version, and platform version", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	inserted, err := store.qs.Query(ctx, qConversationReleasePut,
		release.TenantID, release.ConversationID, release.PlatformVersion, release.ResolvedAt,
		release.TenantID, release.ConversationID, release.AgentVersion,
		release.TenantID, release.ConversationID)
	if err != nil {
		return fmt.Errorf("put conversation release: %w", err)
	}
	if len(inserted.Rows) > 0 {
		return nil
	}
	existing, err := store.Get(ctx, release.TenantID, release.ConversationID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("%w: conversation %s does not exist on agent version %s", domain.ErrConflict, release.ConversationID, release.AgentVersion)
		}
		return err
	}
	if existing.PlatformVersion != release.PlatformVersion || existing.AgentVersion != release.AgentVersion {
		return fmt.Errorf("%w: conversation %s already runs on %s/%s", domain.ErrConflict, release.ConversationID, existing.AgentVersion, existing.PlatformVersion)
	}
	return nil
}

func (store *TableConversationReleaseStore) Migrate(ctx context.Context, tenantID int64, conversationID, platformVersion string, at time.Time) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	result, err := store.qs.Query(ctx, qConversationReleaseMigrate, platformVersion, at, tenantID, conversationID)
	if err != nil {
		return fmt.Errorf("migrate conversation release: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: release for conversation %s", domain.ErrNotFound, conversationID)
	}
	return nil
}

// RingPlatformReleaseResolver assigns a new conversation to the highest live
// ring stage that covers the tenant's ring and wins the traffic hash, else the
// global default.
type RingPlatformReleaseResolver struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.TenantPlatformReleaseResolver = (*RingPlatformReleaseResolver)(nil)

func (resolver *RingPlatformReleaseResolver) init(ctx context.Context) error {
	if resolver.DB == nil {
		return fmt.Errorf("ring platform release resolver: database is required")
	}
	resolver.once.Do(func() { resolver.qs = resolver.DB.GetQueryService(ctx, conversationReleaseQueries) })
	if resolver.qs == nil {
		return fmt.Errorf("ring platform release resolver: query service is required")
	}
	return nil
}

func (resolver *RingPlatformReleaseResolver) Current(ctx context.Context, tenantID int64, conversationID string) (string, error) {
	if err := resolver.init(ctx); err != nil {
		return "", err
	}
	ring, err := resolver.qs.Query(ctx, qTenantRingOrder, tenantID)
	if err != nil {
		return "", fmt.Errorf("tenant ring: %w", err)
	}
	tenantOrder := int64(0)
	if len(ring.Rows) > 0 {
		tenantOrder = common.AsInt64(ring.Rows[0][0])
	}
	candidates, err := resolver.qs.Query(ctx, qLiveReleaseCandidates)
	if err != nil {
		return "", fmt.Errorf("live releases: %w", err)
	}
	fallback := ""
	for _, row := range candidates.Rows {
		version, stage := common.AsString(row[0]), domain.RolloutStage(common.AsString(row[1]))
		percentage, ringOrder := int(common.AsInt64(row[2])), common.AsInt64(row[3])
		if stage == domain.StageGlobalDefault {
			if fallback == "" {
				fallback = version
			}
			continue
		}
		if tenantOrder == 0 || ringOrder == 0 || tenantOrder > ringOrder {
			continue
		}
		if CanarySelected(tenantID, version, conversationID, percentage) {
			return version, nil
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("%w: no global default platform release", domain.ErrNotReady)
	}
	return fallback, nil
}

// StickyReleaseResolver resolves both release identities once, at conversation
// creation, and afterwards only reads them back.
type StickyReleaseResolver struct {
	Store    contract.ConversationReleaseStore
	Platform contract.TenantPlatformReleaseResolver
	Now      func() time.Time
}

func (resolver *StickyReleaseResolver) now() time.Time {
	if resolver.Now != nil {
		return resolver.Now()
	}
	return time.Now()
}

// Resolve returns the conversation's identities. agentVersion is the version the
// conversation was created on; a different persisted version is a conflict, never a switch.
func (resolver *StickyReleaseResolver) Resolve(ctx context.Context, tenantID int64, conversationID, agentVersion string) (domain.ConversationRelease, error) {
	if resolver.Store == nil || resolver.Platform == nil {
		return domain.ConversationRelease{}, fmt.Errorf("sticky release resolver: store and platform resolver are required")
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.ConversationRelease{}, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	existing, err := resolver.Store.Get(ctx, tenantID, conversationID)
	switch {
	case err == nil:
		if agentVersion != "" && existing.AgentVersion != agentVersion {
			return domain.ConversationRelease{}, fmt.Errorf("%w: conversation %s is pinned to agent version %s, not %s", domain.ErrConflict, conversationID, existing.AgentVersion, agentVersion)
		}
		return existing, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.ConversationRelease{}, err
	}
	platform, err := resolver.Platform.Current(ctx, tenantID, conversationID)
	if err != nil {
		return domain.ConversationRelease{}, err
	}
	release := domain.ConversationRelease{
		TenantID: tenantID, ConversationID: conversationID, AgentVersion: agentVersion,
		PlatformVersion: platform, ResolvedAt: resolver.now(),
	}
	if err := resolver.Store.Put(ctx, release); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			if existing, getErr := resolver.Store.Get(ctx, tenantID, conversationID); getErr == nil && existing.AgentVersion == agentVersion {
				return existing, nil
			}
		}
		return domain.ConversationRelease{}, err
	}
	return release, nil
}

// SessionDrainer applies SessionDrainPolicy at turn boundaries: rolled-back
// releases drain within the window, quarantined ones move the conversation to
// a safe release before the next turn, and a running turn is only ever
// cancelled, never spliced onto another release.
type SessionDrainer struct {
	States    contract.RolloutStateStore
	Releases  contract.ConversationReleaseStore
	Platform  contract.TenantPlatformReleaseResolver
	Canceller contract.TurnCanceller
	Audit     contract.AuditSink
	Policy    domain.SessionDrainPolicy
	Now       func() time.Time
}

func (drainer *SessionDrainer) now() time.Time {
	if drainer.Now != nil {
		return drainer.Now()
	}
	return time.Now()
}

func (drainer *SessionDrainer) validate() error {
	if drainer.States == nil || drainer.Releases == nil || drainer.Platform == nil {
		return fmt.Errorf("session drainer: state store, release store, and platform resolver are required")
	}
	if drainer.Policy.Window <= 0 {
		return fmt.Errorf("%w: drain window must be positive", domain.ErrValidation)
	}
	if drainer.Policy.CancelOnCriticalSafety && drainer.Canceller == nil {
		return fmt.Errorf("session drainer: cancelling on critical safety requires a turn canceller")
	}
	return nil
}

// AdmitTurn returns the release the next turn of the conversation runs on,
// migrating it to a safe release when its own release may no longer serve.
func (drainer *SessionDrainer) AdmitTurn(ctx context.Context, release domain.ConversationRelease) (domain.ConversationRelease, error) {
	if err := drainer.validate(); err != nil {
		return domain.ConversationRelease{}, err
	}
	state, err := drainer.States.Get(ctx, release.PlatformVersion)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return release, nil
		}
		return domain.ConversationRelease{}, err
	}
	now := drainer.now()
	switch state.Stage {
	case domain.StageQuarantined:
		return drainer.migrate(ctx, release, "quarantined", now)
	case domain.StageRolledBack:
		if now.Sub(state.StageStartedAt) < drainer.Policy.Window {
			return release, nil
		}
		return drainer.migrate(ctx, release, "drain window elapsed", now)
	}
	return release, nil
}

// Interrupt cancels a running turn whose release was quarantined for critical
// safety; the caller records the explicit partial status and keeps state.
func (drainer *SessionDrainer) Interrupt(ctx context.Context, release domain.ConversationRelease, requestID string) error {
	if err := drainer.validate(); err != nil {
		return err
	}
	if !drainer.Policy.CancelOnCriticalSafety {
		return nil
	}
	state, err := drainer.States.Get(ctx, release.PlatformVersion)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	if state.Stage != domain.StageQuarantined {
		return nil
	}
	reason := "platform release " + release.PlatformVersion + " quarantined"
	if err := drainer.Canceller.Cancel(ctx, release.TenantID, requestID, reason); err != nil {
		return fmt.Errorf("cancel turn %s: %w", requestID, err)
	}
	if err := drainer.audit(ctx, release, "session.turn_cancelled", reason); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", domain.ErrTurnCanceled, reason)
}

func (drainer *SessionDrainer) migrate(ctx context.Context, release domain.ConversationRelease, reason string, now time.Time) (domain.ConversationRelease, error) {
	safe, err := drainer.Platform.Current(ctx, release.TenantID, release.ConversationID)
	if err != nil {
		return domain.ConversationRelease{}, err
	}
	if safe == release.PlatformVersion {
		return domain.ConversationRelease{}, fmt.Errorf("%w: no safe release to drain %s onto", domain.ErrNotReady, release.PlatformVersion)
	}
	if err := drainer.Releases.Migrate(ctx, release.TenantID, release.ConversationID, safe, now); err != nil {
		return domain.ConversationRelease{}, err
	}
	migrated := release
	migrated.PlatformVersion, migrated.ResolvedAt = safe, now
	if err := drainer.audit(ctx, migrated, "session.release_migrated", reason+" from "+release.PlatformVersion); err != nil {
		return domain.ConversationRelease{}, err
	}
	return migrated, nil
}

func (drainer *SessionDrainer) audit(ctx context.Context, release domain.ConversationRelease, category, reason string) error {
	if drainer.Audit == nil {
		return nil
	}
	payload, err := json.Marshal(struct {
		ConversationID  string `json:"conversation_id"`
		AgentVersion    string `json:"agent_version"`
		PlatformVersion string `json:"platform_version"`
		Reason          string `json:"reason"`
	}{release.ConversationID, release.AgentVersion, release.PlatformVersion, reason})
	if err != nil {
		return fmt.Errorf("encode drain audit: %w", err)
	}
	record := domain.DecisionRecord{
		TenantID: release.TenantID, Principal: platformPrincipal, Category: category, Action: "drain",
		Resource: release.PlatformVersion, ReleaseVersion: release.AgentVersion, Outcome: domain.DecisionAllow,
		Reason: reason, ConversationID: release.ConversationID, Payload: payload, OccurredAt: drainer.now(),
	}
	if err := drainer.Audit.Record(ctx, record); err != nil {
		return fmt.Errorf("audit drain: %w", err)
	}
	return nil
}

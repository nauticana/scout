package evaluation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/crypto"
	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qSampleInsert = "scout_evaluation_sample_insert"
	qSampleGet    = "scout_evaluation_sample_get"
	qSampleDelete = "scout_evaluation_sample_delete"
)

var sampleQueries = map[string]string{
	qSampleInsert: `
INSERT INTO evaluation_sample (tenant_id, sample_id, request_id, agent_id, agent_version, reason, risk_bp, uncertainty_bp, redacted,
                               payload_uri, payload_digest, retention_class, region_code, sampled_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qSampleGet: `
SELECT request_id, agent_id, agent_version, reason, risk_bp, uncertainty_bp, redacted, payload_uri, payload_digest,
       retention_class, region_code, sampled_at, expires_at
  FROM evaluation_sample
 WHERE tenant_id = ? AND sample_id = ?`,
	qSampleDelete: `
DELETE FROM evaluation_sample
 WHERE tenant_id = ? AND sample_id = ?
RETURNING payload_uri`,
}

// EncryptedSampleStore keeps sample payloads sealed with AES-256-GCM in object
// storage and their metadata in evaluation_sample; reads are tenant-scoped,
// digest-verified, and refuse expired samples.
type EncryptedSampleStore struct {
	keelStore
	Storage storage.ObjectStorage
	Bucket  string
	// Key is the 32-byte data-encryption key from the keystore, never from config.
	Key []byte
	// Scheme prefixes the payload URI; default "object".
	Scheme string
	// RequireRedacted refuses payloads not marked redacted.
	RequireRedacted bool
	// MaxPayloadBytes bounds one sample; zero means 1 MiB.
	MaxPayloadBytes int
	Now             func() time.Time
}

var _ contract.SampleStore = (*EncryptedSampleStore)(nil)

func (store *EncryptedSampleStore) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}

func (store *EncryptedSampleStore) validate() error {
	if store.Storage == nil || strings.TrimSpace(store.Bucket) == "" || len(store.Key) != 32 {
		return fmt.Errorf("encrypted sample store: object storage, bucket, and a 32-byte key are required")
	}
	if store.MaxPayloadBytes < 0 {
		return fmt.Errorf("%w: max payload bytes cannot be negative", domain.ErrValidation)
	}
	return nil
}

func (store *EncryptedSampleStore) maxPayload() int {
	if store.MaxPayloadBytes > 0 {
		return store.MaxPayloadBytes
	}
	return 1 << 20
}

func (store *EncryptedSampleStore) objectKey(tenantID int64, sampleID string) string {
	return fmt.Sprintf("evaluation/samples/%d/%s", tenantID, sampleID)
}

// Put seals and uploads the payload, then records the metadata row.
func (store *EncryptedSampleStore) Put(ctx context.Context, sample domain.EvaluationSample, payload []byte) (domain.EvaluationSample, error) {
	if err := store.validate(); err != nil {
		return domain.EvaluationSample{}, err
	}
	if sample.TenantID <= 0 || strings.TrimSpace(sample.SampleID) == "" || strings.TrimSpace(sample.RequestID) == "" || strings.TrimSpace(sample.AgentID) == "" || strings.TrimSpace(sample.AgentVersion) == "" {
		return domain.EvaluationSample{}, fmt.Errorf("%w: sample needs tenant, id, request, agent, and version", domain.ErrValidation)
	}
	if strings.ContainsAny(sample.SampleID, "/\\ ") {
		return domain.EvaluationSample{}, fmt.Errorf("%w: sample id cannot contain path separators", domain.ErrValidation)
	}
	if len(payload) == 0 || len(payload) > store.maxPayload() {
		return domain.EvaluationSample{}, fmt.Errorf("%w: payload must be 1..%d bytes", domain.ErrValidation, store.maxPayload())
	}
	if store.RequireRedacted && !sample.Redacted {
		return domain.EvaluationSample{}, fmt.Errorf("%w: sample payload must be redacted before storage", domain.ErrForbidden)
	}
	if strings.TrimSpace(sample.RetentionClass) == "" || strings.TrimSpace(sample.Region) == "" {
		return domain.EvaluationSample{}, fmt.Errorf("%w: retention class and region are required", domain.ErrValidation)
	}
	if sample.SampledAt.IsZero() {
		sample.SampledAt = store.now()
	}
	if !sample.ExpiresAt.After(sample.SampledAt) {
		return domain.EvaluationSample{}, fmt.Errorf("%w: sample must expire after it was taken", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "encrypted sample store", sampleQueries)
	if err != nil {
		return domain.EvaluationSample{}, err
	}
	sealed, err := crypto.Seal(store.Key, payload)
	if err != nil {
		return domain.EvaluationSample{}, fmt.Errorf("seal sample: %w", err)
	}
	key := store.objectKey(sample.TenantID, sample.SampleID)
	if err := store.Storage.Upload(ctx, store.Bucket, key, strings.NewReader(sealed), "application/octet-stream"); err != nil {
		return domain.EvaluationSample{}, fmt.Errorf("upload sample: %w", err)
	}
	scheme := store.Scheme
	if scheme == "" {
		scheme = "object"
	}
	sample.Payload = domain.ObjectRef{URI: scheme + "://" + store.Bucket + "/" + key, Digest: sha256Hex([]byte(sealed))}
	if _, err := qs.Query(ctx, qSampleInsert,
		sample.TenantID, sample.SampleID, sample.RequestID, sample.AgentID, sample.AgentVersion, sample.Reason,
		basisPoints(sample.RiskScore), basisPoints(sample.Uncertainty), sample.Redacted, sample.Payload.URI, sample.Payload.Digest,
		sample.RetentionClass, sample.Region, sample.SampledAt.UTC(), sample.ExpiresAt.UTC(),
	); err != nil {
		_ = store.Storage.Delete(ctx, store.Bucket, key)
		return domain.EvaluationSample{}, fmt.Errorf("insert sample: %w", err)
	}
	return sample, nil
}

// Get returns the metadata and decrypted payload of an unexpired tenant sample.
func (store *EncryptedSampleStore) Get(ctx context.Context, tenantID int64, sampleID string) (domain.EvaluationSample, []byte, error) {
	if err := store.validate(); err != nil {
		return domain.EvaluationSample{}, nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(sampleID) == "" {
		return domain.EvaluationSample{}, nil, fmt.Errorf("%w: tenant and sample id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "encrypted sample store", sampleQueries)
	if err != nil {
		return domain.EvaluationSample{}, nil, err
	}
	result, err := qs.Query(ctx, qSampleGet, tenantID, sampleID)
	if err != nil {
		return domain.EvaluationSample{}, nil, fmt.Errorf("get sample: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.EvaluationSample{}, nil, fmt.Errorf("%w: sample %q", domain.ErrNotFound, sampleID)
	}
	sample, err := decodeSample(tenantID, sampleID, result.Rows[0])
	if err != nil {
		return domain.EvaluationSample{}, nil, err
	}
	if !store.now().Before(sample.ExpiresAt) {
		return domain.EvaluationSample{}, nil, fmt.Errorf("%w: sample %q retention expired", domain.ErrNotFound, sampleID)
	}
	reader, err := store.Storage.Download(ctx, store.Bucket, store.objectKey(tenantID, sampleID))
	if err != nil {
		return domain.EvaluationSample{}, nil, fmt.Errorf("download sample: %w", err)
	}
	defer reader.Close()
	sealed, err := io.ReadAll(io.LimitReader(reader, int64(store.maxPayload())*2+1024))
	if err != nil {
		return domain.EvaluationSample{}, nil, fmt.Errorf("read sample: %w", err)
	}
	if sha256Hex(sealed) != sample.Payload.Digest {
		return domain.EvaluationSample{}, nil, fmt.Errorf("%w: sample %q payload digest mismatch", domain.ErrConflict, sampleID)
	}
	payload, ok := crypto.Open(store.Key, string(bytes.TrimSpace(sealed)))
	if !ok {
		return domain.EvaluationSample{}, nil, fmt.Errorf("%w: sample %q cannot be opened with the configured key", domain.ErrUnauthorized, sampleID)
	}
	return sample, payload, nil
}

// Delete removes the metadata row and the sealed object.
func (store *EncryptedSampleStore) Delete(ctx context.Context, tenantID int64, sampleID string) error {
	if err := store.validate(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(sampleID) == "" {
		return fmt.Errorf("%w: tenant and sample id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "encrypted sample store", sampleQueries)
	if err != nil {
		return err
	}
	result, err := qs.Query(ctx, qSampleDelete, tenantID, sampleID)
	if err != nil {
		return fmt.Errorf("delete sample: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: sample %q", domain.ErrNotFound, sampleID)
	}
	if err := store.Storage.Delete(ctx, store.Bucket, store.objectKey(tenantID, sampleID)); err != nil {
		return fmt.Errorf("delete sample object: %w", err)
	}
	return nil
}

func decodeSample(tenantID int64, sampleID string, row []any) (domain.EvaluationSample, error) {
	if len(row) < 13 {
		return domain.EvaluationSample{}, fmt.Errorf("sample row: expected 13 columns, got %d", len(row))
	}
	sampledAt, ok := common.AsTimeOK(row[11])
	if !ok {
		return domain.EvaluationSample{}, fmt.Errorf("sample row: invalid sampled_at %q", common.AsString(row[11]))
	}
	expiresAt, ok := common.AsTimeOK(row[12])
	if !ok {
		return domain.EvaluationSample{}, fmt.Errorf("sample row: invalid expires_at %q", common.AsString(row[12]))
	}
	return domain.EvaluationSample{
		SampleID: sampleID, TenantID: tenantID, RequestID: common.AsString(row[0]), AgentID: common.AsString(row[1]), AgentVersion: common.AsString(row[2]),
		Reason: common.AsString(row[3]), RiskScore: float64(common.AsInt64(row[4])) / 10000, Uncertainty: float64(common.AsInt64(row[5])) / 10000,
		Redacted: common.AsBool(row[6]), Payload: domain.ObjectRef{URI: common.AsString(row[7]), Digest: common.AsString(row[8])},
		RetentionClass: common.AsString(row[9]), Region: common.AsString(row[10]), SampledAt: sampledAt, ExpiresAt: expiresAt,
	}, nil
}

func basisPoints(value float64) int64 {
	return int64(math.Round(clamp01(value) * 10000))
}

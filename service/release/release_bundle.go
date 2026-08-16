package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qBundlePut = "scout_release_bundle_put"
	qBundleGet = "scout_release_bundle_get"
)

var releaseBundleQueries = map[string]string{
	qBundlePut: `
INSERT INTO release_bundle (platform_version, bundle_digest, signature, signer_key_id, rollback_target, content)
VALUES (?, ?, ?, ?, ?, ?)`,
	qBundleGet: `
SELECT bundle_digest, signature, signer_key_id, rollback_target, content
  FROM release_bundle
 WHERE platform_version = ?`,
}

// bundleContent is the canonical, digest-covered part of a ReleaseBundle;
// signature and digest live beside it, never inside it.
type bundleContent struct {
	PlatformVersion          string                   `json:"platform_version"`
	Versions                 domain.ComponentVersions `json:"versions"`
	ProviderVersion          string                   `json:"provider_version"`
	Tokenizer                string                   `json:"tokenizer"`
	Runtime                  string                   `json:"runtime"`
	DecodingDefaults         json.RawMessage          `json:"decoding_defaults,omitempty"`
	Embedding                string                   `json:"embedding"`
	Reranker                 string                   `json:"reranker"`
	IndexGeneration          string                   `json:"index_generation"`
	ToolVersions             map[string]string        `json:"tool_versions,omitempty"`
	SafetyPolicyVersion      string                   `json:"safety_policy_version"`
	MigrationSet             []string                 `json:"migration_set,omitempty"`
	RollbackTarget           string                   `json:"rollback_target,omitempty"`
	ResidencyPolicy          string                   `json:"residency_policy"`
	Provenance               string                   `json:"provenance"`
	SignerKeyID              string                   `json:"signer_key_id"`
	CompatibilityConstraints json.RawMessage          `json:"compatibility_constraints,omitempty"`
}

// CanonicalBundle returns the canonical JSON and SHA-256 digest of a bundle.
func CanonicalBundle(bundle domain.ReleaseBundle) ([]byte, string, error) {
	content := bundleContent{
		PlatformVersion: bundle.PlatformVersion, Versions: bundle.Versions, ProviderVersion: bundle.ProviderVersion,
		Tokenizer: bundle.Tokenizer, Runtime: bundle.Runtime, Embedding: bundle.Embedding, Reranker: bundle.Reranker,
		IndexGeneration: bundle.IndexGeneration, ToolVersions: bundle.ToolVersions, SafetyPolicyVersion: bundle.SafetyPolicyVersion,
		MigrationSet: bundle.MigrationSet, RollbackTarget: bundle.RollbackTarget, ResidencyPolicy: bundle.ResidencyPolicy,
		Provenance: bundle.Provenance, SignerKeyID: bundle.SignerKeyID,
	}
	var err error
	if content.DecodingDefaults, err = compactJSON(bundle.DecodingDefaults); err != nil {
		return nil, "", fmt.Errorf("%w: decoding defaults: %v", domain.ErrValidation, err)
	}
	if content.CompatibilityConstraints, err = compactJSON(bundle.CompatibilityConstraints); err != nil {
		return nil, "", fmt.Errorf("%w: compatibility constraints: %v", domain.ErrValidation, err)
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, "", fmt.Errorf("encode release bundle: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func compactJSON(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// TableReleaseBundleStore is the keel-backed ReleaseBundleStore over release_bundle.
type TableReleaseBundleStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ReleaseBundleStore = (*TableReleaseBundleStore)(nil)

func (store *TableReleaseBundleStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("release bundle store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, releaseBundleQueries) })
	if store.qs == nil {
		return fmt.Errorf("release bundle store: query service is required")
	}
	return nil
}

func (store *TableReleaseBundleStore) Put(ctx context.Context, bundle domain.ReleaseBundle) error {
	switch {
	case strings.TrimSpace(bundle.PlatformVersion) == "" || bundle.Versions.Model == "" || bundle.SafetyPolicyVersion == "":
		return fmt.Errorf("%w: bundle needs a platform version, model version, and safety policy version", domain.ErrValidation)
	case strings.TrimSpace(bundle.Signature) == "" || strings.TrimSpace(bundle.SignerKeyID) == "":
		return fmt.Errorf("%w: bundle must be signed", domain.ErrValidation)
	case bundle.RollbackTarget == bundle.PlatformVersion:
		return fmt.Errorf("%w: bundle cannot roll back to itself", domain.ErrValidation)
	}
	content, digest, err := CanonicalBundle(bundle)
	if err != nil {
		return err
	}
	if bundle.Digest != "" && bundle.Digest != digest {
		return fmt.Errorf("%w: bundle digest %s does not match content %s", domain.ErrConflict, bundle.Digest, digest)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	if _, err := store.qs.Query(ctx, qBundlePut, bundle.PlatformVersion, digest, bundle.Signature, bundle.SignerKeyID, nullableString(bundle.RollbackTarget), string(content)); err != nil {
		return fmt.Errorf("put release bundle: %w", err)
	}
	return nil
}

func (store *TableReleaseBundleStore) Get(ctx context.Context, platformVersion string) (domain.ReleaseBundle, error) {
	if err := store.init(ctx); err != nil {
		return domain.ReleaseBundle{}, err
	}
	result, err := store.qs.Query(ctx, qBundleGet, platformVersion)
	if err != nil {
		return domain.ReleaseBundle{}, fmt.Errorf("get release bundle: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.ReleaseBundle{}, fmt.Errorf("%w: release bundle %s", domain.ErrNotFound, platformVersion)
	}
	row := result.Rows[0]
	var content bundleContent
	if err := json.Unmarshal([]byte(common.AsString(row[4])), &content); err != nil {
		return domain.ReleaseBundle{}, fmt.Errorf("decode release bundle: %w", err)
	}
	bundle := domain.ReleaseBundle{
		PlatformVersion: content.PlatformVersion, Versions: content.Versions, ProviderVersion: content.ProviderVersion,
		Tokenizer: content.Tokenizer, Runtime: content.Runtime, DecodingDefaults: content.DecodingDefaults,
		Embedding: content.Embedding, Reranker: content.Reranker, IndexGeneration: content.IndexGeneration,
		ToolVersions: content.ToolVersions, SafetyPolicyVersion: content.SafetyPolicyVersion, MigrationSet: content.MigrationSet,
		RollbackTarget: content.RollbackTarget, ResidencyPolicy: content.ResidencyPolicy, Provenance: content.Provenance,
		Signature: common.AsString(row[1]), SignerKeyID: content.SignerKeyID, CompatibilityConstraints: content.CompatibilityConstraints,
		Digest: common.AsString(row[0]),
	}
	_, digest, err := CanonicalBundle(bundle)
	if err != nil {
		return domain.ReleaseBundle{}, err
	}
	if digest != bundle.Digest || bundle.PlatformVersion != platformVersion || common.AsString(row[3]) != content.RollbackTarget || common.AsString(row[2]) != content.SignerKeyID {
		return domain.ReleaseBundle{}, fmt.Errorf("%w: release bundle %s content does not match its digest", domain.ErrConflict, platformVersion)
	}
	return bundle, nil
}

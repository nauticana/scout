// Package evaluation implements Scout's quality evaluation pipeline: immutable
// manifests, golden sets, paired baseline/candidate scoring, pluggable
// evaluators, signed gate decisions, production sampling, calibration, and
// retrieval evaluation.
package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nauticana/scout/domain"
)

// ManifestBuilder validates a manifest draft and stamps its content-addressed id.
type ManifestBuilder struct {
	Now func() time.Time
}

func (builder *ManifestBuilder) now() time.Time {
	if builder.Now != nil {
		return builder.Now()
	}
	return time.Now()
}

// Build returns the immutable manifest for a draft; ManifestID and CreatedAt of the draft are ignored.
func (builder *ManifestBuilder) Build(draft domain.EvaluationManifest) (domain.EvaluationManifest, error) {
	manifest, err := normalizeManifest(draft)
	if err != nil {
		return domain.EvaluationManifest{}, err
	}
	id, err := manifestID(manifest)
	if err != nil {
		return domain.EvaluationManifest{}, err
	}
	manifest.ManifestID = id
	manifest.CreatedAt = builder.now().UTC()
	return manifest, nil
}

// Verify reports whether the manifest id still matches its content.
func (builder *ManifestBuilder) Verify(manifest domain.EvaluationManifest) error {
	normalized, err := normalizeManifest(manifest)
	if err != nil {
		return err
	}
	id, err := manifestID(normalized)
	if err != nil {
		return err
	}
	if id != manifest.ManifestID {
		return fmt.Errorf("%w: manifest id %q does not match content %q", domain.ErrConflict, manifest.ManifestID, id)
	}
	return nil
}

// DatasetRevision digests the identity, payload digest, expectation, and scope of every example.
func DatasetRevision(examples []domain.GoldenExample) string {
	lines := make([]string, 0, len(examples))
	for _, example := range examples {
		lines = append(lines, strings.Join([]string{
			example.ExampleID, example.Payload.Digest, sha256Hex(example.ExpectedBehavior), fmt.Sprint(example.Hidden),
		}, "\x1f"))
	}
	sort.Strings(lines)
	return sha256Hex([]byte(strings.Join(lines, "\x1e")))
}

func normalizeManifest(draft domain.EvaluationManifest) (domain.EvaluationManifest, error) {
	manifest := draft
	manifest.ManifestID = ""
	manifest.CreatedAt = time.Time{}
	if manifest.TenantID <= 0 {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: manifest tenant is required", domain.ErrValidation)
	}
	for _, subject := range []struct {
		role    domain.EvaluationRole
		subject domain.EvaluationSubject
	}{{domain.RoleCandidate, manifest.Candidate}, {domain.RoleBaseline, manifest.Baseline}} {
		if strings.TrimSpace(subject.subject.AgentID) == "" || strings.TrimSpace(subject.subject.AgentVersion) == "" {
			return domain.EvaluationManifest{}, fmt.Errorf("%w: %s agent id and version are required", domain.ErrValidation, subject.role)
		}
		if subject.subject.IndexGeneration < 0 {
			return domain.EvaluationManifest{}, fmt.Errorf("%w: %s index generation cannot be negative", domain.ErrValidation, subject.role)
		}
	}
	if manifest.Candidate.AgentID != manifest.Baseline.AgentID {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: candidate and baseline must evaluate the same agent", domain.ErrValidation)
	}
	if strings.TrimSpace(manifest.GoldenSetID) == "" || manifest.GoldenSetVersion <= 0 {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: golden set id and positive version are required", domain.ErrValidation)
	}
	if !isSHA256Hex(manifest.DatasetRevision) {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: dataset revision must be a sha-256 hex digest", domain.ErrValidation)
	}
	if len(manifest.Evaluators) == 0 {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: at least one evaluator version is required", domain.ErrValidation)
	}
	if strings.TrimSpace(manifest.SafetyPolicyVersion) == "" {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: safety policy version is required", domain.ErrValidation)
	}
	evaluators := append([]domain.EvaluatorVersion(nil), manifest.Evaluators...)
	for _, evaluator := range evaluators {
		if strings.TrimSpace(evaluator.Kind) == "" || strings.TrimSpace(evaluator.Version) == "" {
			return domain.EvaluationManifest{}, fmt.Errorf("%w: evaluator kind and version are required", domain.ErrValidation)
		}
	}
	sort.Slice(evaluators, func(i, j int) bool {
		if evaluators[i].Kind != evaluators[j].Kind {
			return evaluators[i].Kind < evaluators[j].Kind
		}
		return evaluators[i].Version < evaluators[j].Version
	})
	manifest.Evaluators = evaluators
	var err error
	if manifest.Candidate.Decoding, err = canonicalDecoding(manifest.Candidate.Decoding); err != nil {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: candidate decoding: %v", domain.ErrValidation, err)
	}
	if manifest.Baseline.Decoding, err = canonicalDecoding(manifest.Baseline.Decoding); err != nil {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: baseline decoding: %v", domain.ErrValidation, err)
	}
	return manifest, nil
}

func canonicalDecoding(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func manifestID(manifest domain.EvaluationManifest) (string, error) {
	canonical, err := canonicalJSON(manifest)
	if err != nil {
		return "", fmt.Errorf("canonical manifest: %w", err)
	}
	return sha256Hex(canonical), nil
}

// canonicalJSON re-encodes any value with sorted object keys and no whitespace.
func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

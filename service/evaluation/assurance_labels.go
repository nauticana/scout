package evaluation

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/domain"
)

// DefaultCaseMaxLabels bounds the ids one case labels in each evidence role.
const DefaultCaseMaxLabels = 50

// maxEvidenceIDLength is golden_case_evidence.evidence_id's width.
const maxEvidenceIDLength = 200

// validAssurance trims, sorts, and bounds a case's labels; a blank, oversized, or repeated id is refused.
func validAssurance(labels domain.AssuranceLabels, maxLabels int) (domain.AssuranceLabels, error) {
	var err error
	if labels.RequiredEvidence, err = validEvidenceIDs("required evidence", labels.RequiredEvidence, maxLabels); err != nil {
		return labels, err
	}
	if labels.ControllingAuthority, err = validEvidenceIDs("controlling authority", labels.ControllingAuthority, maxLabels); err != nil {
		return labels, err
	}
	return labels, nil
}

func validEvidenceIDs(role string, ids []string, maxLabels int) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxLabels {
		return nil, fmt.Errorf("%w: a case labels at most %d %s ids", domain.ErrValidation, maxLabels, role)
	}
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > maxEvidenceIDLength || !validText([]byte(id)) {
			return nil, fmt.Errorf("%w: a %s id is 1 to %d bytes of UTF-8", domain.ErrValidation, role, maxEvidenceIDLength)
		}
		normalized = append(normalized, id)
	}
	slices.Sort(normalized)
	if len(slices.Compact(slices.Clone(normalized))) != len(normalized) {
		return nil, fmt.Errorf("%w: a %s id is labeled twice", domain.ErrValidation, role)
	}
	return normalized, nil
}

// assuranceColumns are golden_example's required_evidence_ids, controlling_authority_ids, and execution_expected.
func assuranceColumns(labels domain.AssuranceLabels) []any {
	columns := []any{jsonIDs(labels.RequiredEvidence), jsonIDs(labels.ControllingAuthority), nil}
	if labels.ExecutionExpected != nil {
		columns[2] = *labels.ExecutionExpected
	}
	return columns
}

func jsonIDs(ids []string) any {
	if len(ids) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(ids) // a string slice always encodes
	return string(encoded)
}

// assuranceDigest identifies a labeled example's labels for its dataset revision.
func assuranceDigest(labels domain.AssuranceLabels) string {
	encoded, _ := json.Marshal(labels) // strings and a bool always encode
	return sha256Hex(encoded)
}

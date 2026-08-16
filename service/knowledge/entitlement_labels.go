package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nauticana/scout/domain"
)

// Entitlement scheme: a chunk carries the JSON array of opaque labels that
// grant it (e.g. "user:42", "group:finance"); a query carries the labels its
// principal holds. A chunk is visible when the two sets intersect, so an empty
// side on either end can never match — that is what makes the scheme fail closed.

// ParseEntitlements decodes a JSON label array; nil, empty, or malformed input fails closed.
func ParseEntitlements(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: entitlements are required", domain.ErrForbidden)
	}
	var labels []string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, fmt.Errorf("%w: entitlements must be a JSON array of labels: %v", domain.ErrForbidden, err)
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: entitlements are empty", domain.ErrForbidden)
	}
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("%w: entitlement labels cannot be blank", domain.ErrForbidden)
		}
	}
	return labels, nil
}

// EncodeEntitlements returns the canonical (sorted, de-duplicated) JSON label array.
func EncodeEntitlements(labels []string) ([]byte, error) {
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: at least one entitlement label is required", domain.ErrValidation)
	}
	unique := make(map[string]struct{}, len(labels))
	canonical := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("%w: entitlement labels cannot be blank", domain.ErrValidation)
		}
		if _, seen := unique[label]; seen {
			continue
		}
		unique[label] = struct{}{}
		canonical = append(canonical, label)
	}
	sort.Strings(canonical)
	return json.Marshal(canonical)
}

// EntitlementsDigest returns the lowercase SHA-256 hex a KnowledgeQuery must carry for its Entitlements bytes.
func EntitlementsDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Entitled reports whether the principal holds any label granting the chunk — the same any-of rule the SQL predicate applies.
func Entitled(chunkLabels, principalLabels []string) bool {
	if len(chunkLabels) == 0 || len(principalLabels) == 0 {
		return false
	}
	held := make(map[string]struct{}, len(principalLabels))
	for _, label := range principalLabels {
		held[label] = struct{}{}
	}
	for _, label := range chunkLabels {
		if _, ok := held[label]; ok {
			return true
		}
	}
	return false
}

// authorizeQuery is the fail-closed gate every retrieval leg runs before touching the index.
func authorizeQuery(query domain.KnowledgeQuery) ([]string, error) {
	if query.TenantContext.TenantID <= 0 || strings.TrimSpace(query.KnowledgeBaseID) == "" || strings.TrimSpace(query.KnowledgeVersion) == "" {
		return nil, fmt.Errorf("%w: tenant, knowledge base, and knowledge version are required", domain.ErrValidation)
	}
	if query.TopK <= 0 {
		return nil, fmt.Errorf("%w: positive TopK is required", domain.ErrValidation)
	}
	labels, err := ParseEntitlements(query.Entitlements)
	if err != nil {
		return nil, err
	}
	if query.EntitlementsDigest != EntitlementsDigest(query.Entitlements) {
		return nil, fmt.Errorf("%w: entitlements digest is stale", domain.ErrForbidden)
	}
	return labels, nil
}

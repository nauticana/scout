package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RedactionPolicy is a versioned field allowlist: fields it does not allow
// have their values masked before the chunk is embedded or stored.
type RedactionPolicy interface {
	Version() string
	Allows(field string) bool
}

// AllowlistPolicy is a RedactionPolicy over an explicit field set.
type AllowlistPolicy struct {
	PolicyVersion string
	Fields        []string
}

var _ RedactionPolicy = (*AllowlistPolicy)(nil)

func (policy *AllowlistPolicy) Version() string { return policy.PolicyVersion }

func (policy *AllowlistPolicy) Allows(field string) bool { return containsFold(policy.Fields, field) }

// PolicyRedactor masks the value of every "field: value" line whose field the
// chunk's policy version does not allow. A chunk without a policy version
// passes through; an unknown version fails closed.
type PolicyRedactor struct {
	Policies []RedactionPolicy
	// Mask replaces a redacted value; default "[redacted]".
	Mask string
}

var _ contract.ChunkRedactor = (*PolicyRedactor)(nil)

const defaultRedactionMask = "[redacted]"

// Redact returns the derivative chunk with masked values and a recomputed content digest.
func (redactor *PolicyRedactor) Redact(_ context.Context, chunk domain.KnowledgeChunk) (domain.KnowledgeChunk, error) {
	if chunk.RedactionPolicyVersion == "" {
		return chunk, nil
	}
	policy := redactor.policy(chunk.RedactionPolicyVersion)
	if policy == nil {
		return domain.KnowledgeChunk{}, fmt.Errorf("%w: redaction policy version %q is unknown", domain.ErrValidation, chunk.RedactionPolicyVersion)
	}
	mask := redactor.Mask
	if mask == "" {
		mask = defaultRedactionMask
	}
	lines := bytes.Split(chunk.Content, []byte("\n"))
	for i, line := range lines {
		field, ok := fieldName(line)
		if ok && !policy.Allows(field) {
			lines[i] = append(append([]byte(nil), line[:len(field)+1]...), []byte(" "+mask)...)
		}
	}
	chunk.Content = bytes.Join(lines, []byte("\n"))
	chunk.ContentDigest = sha256Bytes(chunk.Content)
	return chunk, nil
}

func (redactor *PolicyRedactor) policy(version string) RedactionPolicy {
	for _, policy := range redactor.Policies {
		if policy != nil && policy.Version() == version {
			return policy
		}
	}
	return nil
}

// fieldName returns the "name" of a "name: value" line; names carry no whitespace and are followed by a value.
func fieldName(line []byte) (string, bool) {
	name, value, ok := strings.Cut(string(line), ":")
	if !ok || name == "" || strings.TrimSpace(value) == "" || strings.ContainsAny(name, " \t") {
		return "", false
	}
	return name, true
}

// Package observability bounds what leaves the process as fleet metrics:
// allowlisted labels, a fixed metric catalog, sketch-based tenant views, and
// redacted audit trails for rejected or failed observations.
package observability

import (
	"fmt"
	"sort"

	"github.com/nauticana/scout/domain"
)

// Allowlisted label keys; every fleet series is built only from these.
const (
	LabelTenantTier    = "tenant_tier"
	LabelPriorityClass = "priority_class"
	LabelModel         = "model"
	LabelProvider      = "provider"
	LabelRegion        = "region"
	LabelStage         = "stage"
	LabelComponent     = "component"
	LabelRelease       = "release"
	LabelOutcome       = "outcome"
	LabelErrorClass    = "error_class"
	LabelVerdict       = "verdict"
	LabelTenantRank    = "tenant_rank"
	LabelCurrency      = "currency"
)

// Forbidden label keys name identity or free text; a policy refuses them even if allowlisted.
const (
	LabelTenantID       = "tenant_id"
	LabelRequestID      = "request_id"
	LabelConversationID = "conversation_id"
)

var defaultAllowed = []string{
	LabelTenantTier, LabelPriorityClass, LabelModel, LabelProvider, LabelRegion, LabelStage,
	LabelComponent, LabelRelease, LabelOutcome, LabelErrorClass, LabelVerdict, LabelTenantRank, LabelCurrency,
}

var defaultForbidden = []string{
	LabelTenantID, LabelRequestID, LabelConversationID,
	"prompt", "response", "input", "output", "query", "message", "text", "payload", "document_id", "chunk_id",
	"principal", "user", "user_id", "email", "trace_id", "reservation_id",
}

// DefaultMaxLabelValueLength bounds one label value; long values are free text, not dimensions.
const DefaultMaxLabelValueLength = 64

// LabelPolicy is the allowlist and value bounds every fleet label set must satisfy.
type LabelPolicy struct {
	// Allowed keys; empty uses the package default allowlist.
	Allowed []string
	// Forbidden keys are refused even when a caller adds them to Allowed.
	Forbidden []string
	// MaxValueLength bounds each value's byte length; zero uses DefaultMaxLabelValueLength.
	MaxValueLength int
}

// DefaultLabelPolicy is the package allowlist with default bounds.
func DefaultLabelPolicy() LabelPolicy {
	return LabelPolicy{Allowed: defaultAllowed, Forbidden: defaultForbidden, MaxValueLength: DefaultMaxLabelValueLength}
}

// Validate rejects a policy whose allowlist is empty or overlaps its forbidden set.
func (policy LabelPolicy) Validate() error {
	if len(policy.allowed()) == 0 {
		return fmt.Errorf("label policy: allowlist cannot be empty")
	}
	if policy.MaxValueLength < 0 {
		return fmt.Errorf("label policy: max value length cannot be negative")
	}
	forbidden := policy.forbidden()
	for _, key := range policy.allowed() {
		if _, refused := forbidden[key]; refused {
			return fmt.Errorf("label policy: %q is both allowed and forbidden", key)
		}
	}
	return nil
}

// Sanitize returns a copy of labels or an ErrValidation error naming the first
// forbidden or unknown key, over-long value, or out-of-charset value.
func (policy LabelPolicy) Sanitize(labels map[string]string) (map[string]string, error) {
	if err := policy.compile().validate(labels); err != nil {
		return nil, err
	}
	clean := make(map[string]string, len(labels))
	for key, value := range labels {
		clean[key] = value
	}
	return clean, nil
}

// compiledPolicy is the hot-path form; the metrics recorder compiles once.
type compiledPolicy struct {
	allowed   map[string]struct{}
	forbidden map[string]struct{}
	maxLength int
}

func (policy LabelPolicy) compile() compiledPolicy {
	allowed := make(map[string]struct{}, len(policy.allowed()))
	for _, key := range policy.allowed() {
		allowed[key] = struct{}{}
	}
	maxLength := policy.MaxValueLength
	if maxLength <= 0 {
		maxLength = DefaultMaxLabelValueLength
	}
	return compiledPolicy{allowed: allowed, forbidden: policy.forbidden(), maxLength: maxLength}
}

// validate reports the first violation in a deterministic key order.
func (policy compiledPolicy) validate(labels map[string]string) error {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, refused := policy.forbidden[key]; refused {
			return fmt.Errorf("%w: label %q is forbidden", domain.ErrValidation, key)
		}
		if _, ok := policy.allowed[key]; !ok {
			return fmt.Errorf("%w: label %q is not allowlisted", domain.ErrValidation, key)
		}
		value := labels[key]
		if len(value) > policy.maxLength {
			return fmt.Errorf("%w: label %q value exceeds %d bytes", domain.ErrValidation, key, policy.maxLength)
		}
		if !labelValueCharset(value) {
			return fmt.Errorf("%w: label %q value has characters outside [A-Za-z0-9._:/-]", domain.ErrValidation, key)
		}
	}
	return nil
}

func (policy LabelPolicy) allowed() []string {
	if len(policy.Allowed) == 0 {
		return defaultAllowed
	}
	return policy.Allowed
}

func (policy LabelPolicy) forbidden() map[string]struct{} {
	keys := policy.Forbidden
	if keys == nil {
		keys = defaultForbidden
	}
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

// labelValueCharset admits identifiers, versions, and paths; whitespace and punctuation mark free text.
func labelValueCharset(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '/', c == '-':
		default:
			return false
		}
	}
	return true
}

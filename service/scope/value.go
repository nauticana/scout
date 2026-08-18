// Package scope compiles a tenant's configuration hierarchy into the immutable
// effective release the runtime pins. Bindings carry canonical JSON values whose
// shape is fixed per resource kind:
//
//	set kinds (tool, knowledge, model, entitlement)          ["a","b"]
//	policy                                                   [{"id":"…","effect":"allow","actions":[…],"resources":[…]}]
//	prompt_section                                           {"instruction":"…","output":"…"}
//	budget                                                   {"tokens":0,"cost_minor_units":0,"currency":"EUR"}
//	autonomy                                                 {"mode":"draft"}
package scope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nauticana/scout/domain"
)

// autonomyRank orders the operating modes; a child may only lower the rank.
var autonomyRank = map[domain.AutonomyMode]int{
	domain.AutonomyHumanOnly:           0,
	domain.AutonomyAdvise:              1,
	domain.AutonomyDraft:               2,
	domain.AutonomyExecuteWithApproval: 3,
	domain.AutonomyBounded:             4,
}

type promptValue struct {
	Instruction string `json:"instruction"`
	Output      string `json:"output,omitempty"`
}

type budgetValue struct {
	Tokens         int64  `json:"tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
}

type autonomyValue struct {
	Mode       domain.AutonomyMode `json:"mode"`
	WindowFrom int                 `json:"window_from_minute,omitempty"`
	WindowTo   int                 `json:"window_to_minute,omitempty"`
}

func decodeSet(raw []byte) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var items []string
	if err := strictDecode(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: set value must be a JSON array of strings: %v", domain.ErrValidation, err)
	}
	return items, nil
}

func encodeSet(items []string) ([]byte, error) {
	unique := make(map[string]struct{}, len(items))
	ordered := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return nil, fmt.Errorf("%w: set value contains a blank member", domain.ErrValidation)
		}
		if _, seen := unique[item]; seen {
			continue
		}
		unique[item] = struct{}{}
		ordered = append(ordered, item)
	}
	sort.Strings(ordered)
	return json.Marshal(ordered)
}

func decodeStatements(raw []byte) ([]domain.PolicyStatement, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var statements []domain.PolicyStatement
	if err := json.Unmarshal(raw, &statements); err != nil {
		return nil, fmt.Errorf("%w: policy value must be a JSON statement list: %v", domain.ErrValidation, err)
	}
	seen := make(map[string]struct{}, len(statements))
	for _, statement := range statements {
		if strings.TrimSpace(statement.ID) == "" {
			return nil, fmt.Errorf("%w: every policy statement needs an id", domain.ErrValidation)
		}
		if _, duplicate := seen[statement.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate policy statement id %q", domain.ErrValidation, statement.ID)
		}
		seen[statement.ID] = struct{}{}
		if statement.Effect != domain.PolicyAllow && statement.Effect != domain.PolicyDeny {
			return nil, fmt.Errorf("%w: statement %q has invalid effect %q", domain.ErrValidation, statement.ID, statement.Effect)
		}
	}
	return statements, nil
}

func encodeStatements(statements []domain.PolicyStatement) ([]byte, error) {
	seen := make(map[string]struct{}, len(statements))
	ordered := make([]domain.PolicyStatement, 0, len(statements))
	for _, statement := range statements {
		if _, duplicate := seen[statement.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate policy statement id %q", domain.ErrValidation, statement.ID)
		}
		seen[statement.ID] = struct{}{}
		ordered = append(ordered, statement)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	return json.Marshal(ordered)
}

func decodePrompt(raw []byte) (promptValue, error) {
	var value promptValue
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, nil
	}
	if err := strictDecode(raw, &value); err != nil {
		return value, fmt.Errorf("%w: prompt value must be an instruction/output object: %v", domain.ErrValidation, err)
	}
	return value, nil
}

func decodeBudget(raw []byte) (budgetValue, error) {
	var value budgetValue
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, nil
	}
	if err := strictDecode(raw, &value); err != nil {
		return value, fmt.Errorf("%w: budget value must be a token/cost object: %v", domain.ErrValidation, err)
	}
	if value.Tokens < 0 || value.CostMinorUnits < 0 {
		return value, fmt.Errorf("%w: budget limits cannot be negative", domain.ErrValidation)
	}
	return value, nil
}

func decodeAutonomy(raw []byte) (autonomyValue, error) {
	var value autonomyValue
	if err := strictDecode(raw, &value); err != nil {
		return value, fmt.Errorf("%w: autonomy value must be a mode object: %v", domain.ErrValidation, err)
	}
	if _, known := autonomyRank[value.Mode]; !known {
		return value, fmt.Errorf("%w: unknown autonomy mode %q", domain.ErrValidation, value.Mode)
	}
	if value.WindowFrom < 0 || value.WindowFrom >= 1440 || value.WindowTo < 0 || value.WindowTo > 1440 ||
		(value.WindowTo == 0 && value.WindowFrom != 0) || (value.WindowTo > 0 && value.WindowFrom >= value.WindowTo) {
		return value, fmt.Errorf("%w: autonomy operating window must be a non-empty UTC minute range", domain.ErrValidation)
	}
	return value, nil
}

func strictDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing content after the JSON value")
	}
	return nil
}

// Digest returns the canonical digest of one compiled release. It covers every
// effective value and its winning provenance, so two releases with the same
// digest resolve identically.
func Digest(release domain.EffectiveRelease) string {
	var payload strings.Builder
	payload.WriteString(digestVersion)
	payload.WriteByte('\n')
	writeField(&payload, fmt.Sprintf("%d", release.TenantID))
	writeField(&payload, release.AgentID)
	writeField(&payload, release.AgentVersion)
	writeField(&payload, release.ScopeID)
	writeField(&payload, release.CompiledAt.UTC().Format(time.RFC3339Nano))
	if release.CompiledBy != nil {
		writeField(&payload, fmt.Sprintf("%d", *release.CompiledBy))
	} else {
		writeField(&payload, "")
	}
	for _, resource := range release.Resources {
		writeField(&payload, string(resource.ResourceKind))
		writeField(&payload, resource.ResourceID)
		writeField(&payload, string(resource.Value))
		writeProvenance(&payload, resource.Source)
		for _, superseded := range resource.Superseded {
			writeProvenance(&payload, superseded)
		}
		payload.WriteByte(0x1e)
	}
	sum := sha256.Sum256([]byte(payload.String()))
	return hex.EncodeToString(sum[:])
}

func writeProvenance(payload *strings.Builder, provenance domain.Provenance) {
	writeField(payload, provenance.ScopeID)
	writeField(payload, provenance.ScopeKind)
	writeField(payload, string(provenance.ResourceKind))
	writeField(payload, provenance.ResourceID)
	writeField(payload, provenance.ResourceVersion)
	writeField(payload, string(provenance.MergeMode))
	writeField(payload, fmt.Sprintf("%t", provenance.Sealed))
	if provenance.Approver != nil {
		writeField(payload, fmt.Sprintf("%d", *provenance.Approver))
	} else {
		writeField(payload, "")
	}
	writeField(payload, provenance.CompiledAt.UTC().Format(time.RFC3339Nano))
}

const digestVersion = "scout.effective_release.v1"

func writeField(payload *strings.Builder, value string) {
	fmt.Fprintf(payload, "%d:%s", len(value), value)
	payload.WriteByte(0x1f)
}

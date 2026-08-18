package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	// DefaultUntrustedOpen and DefaultUntrustedClose fence tool and retrieved content re-entering the graph.
	DefaultUntrustedOpen  = "<untrusted_content>"
	DefaultUntrustedClose = "</untrusted_content>"
	// RedactionMask replaces every redacted span.
	RedactionMask = "[REDACTED]"
	// AuditCategory labels guardrail audit events.
	AuditCategory = domain.DecisionCategoryGuardrail
	// ErrorCodePolicyViolation is the terminal reply error code for a blocked stream.
	ErrorCodePolicyViolation = string(domain.StageGuardrail)
)

// ErrApprovalPending marks an irreversible tool call waiting on a decision. It
// wraps domain.ErrApprovalPending, not ErrForbidden: the turn suspends rather
// than failing, and resumes when the verdict arrives.
var ErrApprovalPending = fmt.Errorf("%w: tool approval pending", domain.ErrApprovalPending)

// ViolationError is the typed outcome of a blocking rule; it unwraps to domain.ErrForbidden and carries no content.
type ViolationError struct {
	Stage         domain.GuardrailStage
	RuleIDs       []string
	Severity      domain.GuardrailSeverity
	PolicyVersion string
	Err           error
}

func (e *ViolationError) Error() string {
	return fmt.Sprintf("guardrail %s blocked by rules %s (policy %s)", e.Stage, strings.Join(e.RuleIDs, ","), e.PolicyVersion)
}

func (e *ViolationError) Unwrap() error { return e.Err }

// EnforcerConfig wires the layered enforcer; Baseline, Compiler, and Events are required.
type EnforcerConfig struct {
	Baseline    domain.GuardrailRuleSet
	Compiler    *RuleSetCompiler
	Classifiers map[domain.GuardrailRuleKind]contract.ClassifierProvider
	// Approvals is consulted by irreversible_tool_approval rules; absent while such a rule exists fails closed.
	Approvals contract.ToolApprovalGate
	Events    contract.SafetyEventSink
	// Audit optionally receives a redacted copy of every safety event.
	Audit contract.AuditSink
	// MaxChunkBytes bounds one streamed chunk accepted by an output session; default 64 KiB.
	MaxChunkBytes int
	Now           func() time.Time
}

// LayeredEnforcer composes the release-independent baseline with the pinned release policy;
// every rule of both layers runs, so release rules strengthen but can never disable the baseline.
type LayeredEnforcer struct {
	baseline      *CompiledRuleSet
	compiler      *RuleSetCompiler
	classifiers   map[domain.GuardrailRuleKind]contract.ClassifierProvider
	approvals     contract.ToolApprovalGate
	events        contract.SafetyEventSink
	audit         contract.AuditSink
	maxChunkBytes int
	now           func() time.Time
}

var (
	_ contract.GuardrailEnforcer  = (*LayeredEnforcer)(nil)
	_ contract.StreamingGuardrail = (*LayeredEnforcer)(nil)
)

// NewLayeredEnforcer compiles the baseline and verifies that every baseline classifier has a provider.
func NewLayeredEnforcer(config EnforcerConfig) (*LayeredEnforcer, error) {
	if config.Compiler == nil || config.Events == nil {
		return nil, fmt.Errorf("layered enforcer: compiler and safety event sink are required")
	}
	if config.MaxChunkBytes < 0 {
		return nil, fmt.Errorf("layered enforcer: max chunk bytes cannot be negative")
	}
	baseline, err := config.Compiler.CompileBaseline(config.Baseline)
	if err != nil {
		return nil, fmt.Errorf("layered enforcer: baseline: %w", err)
	}
	for kind := range baseline.classifierKinds {
		if config.Classifiers[kind] == nil {
			return nil, fmt.Errorf("layered enforcer: baseline needs a %s classifier provider", kind)
		}
	}
	enforcer := &LayeredEnforcer{
		baseline: baseline, compiler: config.Compiler, classifiers: config.Classifiers,
		approvals: config.Approvals, events: config.Events, audit: config.Audit,
		maxChunkBytes: config.MaxChunkBytes, now: config.Now,
	}
	if enforcer.maxChunkBytes == 0 {
		enforcer.maxChunkBytes = 64 << 10
	}
	if enforcer.now == nil {
		enforcer.now = time.Now
	}
	return enforcer, nil
}

// BeforeModel applies input-stage rules to the prompt.
func (enforcer *LayeredEnforcer) BeforeModel(ctx context.Context, config domain.GuardrailConfig, request domain.ModelRequest) (domain.ModelRequest, error) {
	subject := domain.GuardrailSubject{TenantID: request.TenantContext.TenantID, Principal: request.Principal, RequestID: request.RequestID, ConversationID: request.ConversationID}
	content, _, err := enforcer.inspect(ctx, config, &inspection{stage: domain.GuardrailStageInput, subject: subject, content: request.Prompt, sizeBytes: len(request.Prompt)})
	if err != nil {
		return domain.ModelRequest{}, err
	}
	request.Prompt = content
	return request, nil
}

// AfterModelChunk applies output-stage rules to one chunk in isolation; prefer OpenOutputSession for cross-chunk state.
func (enforcer *LayeredEnforcer) AfterModelChunk(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
	content, _, err := enforcer.inspect(ctx, config, &inspection{stage: domain.GuardrailStageOutput, subject: subject, content: chunk.Payload, sizeBytes: len(chunk.Payload)})
	if err != nil {
		return domain.ModelChunk{}, err
	}
	chunk.Payload = content
	return chunk, nil
}

// BeforeTool applies tool-input rules to the call and its arguments; blocked arguments never leave.
func (enforcer *LayeredEnforcer) BeforeTool(ctx context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error) {
	subject := domain.GuardrailSubject{TenantID: call.TenantContext.TenantID, Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID}, RequestID: call.RequestID, ConversationID: call.ConversationID, ReleaseVersion: call.Principal.Release}
	content, _, err := enforcer.inspect(ctx, config, &inspection{stage: domain.GuardrailStageToolInput, subject: subject, content: call.Arguments, sizeBytes: len(call.Arguments), call: &call})
	if err != nil {
		return domain.ToolCall{}, err
	}
	call.Arguments = content
	return call, nil
}

// AfterTool applies tool-output rules and marks the output untrusted before it re-enters the graph.
func (enforcer *LayeredEnforcer) AfterTool(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject, result domain.ToolResult) (domain.ToolResult, error) {
	content, _, err := enforcer.inspect(ctx, config, &inspection{stage: domain.GuardrailStageToolOutput, subject: subject, content: result.Output, sizeBytes: len(result.Output)})
	if err != nil {
		return domain.ToolResult{}, err
	}
	result.Output = content
	return result, nil
}

// InspectRetrieved applies retrieval-stage rules to knowledge content, marks it untrusted, and
// returns the verdict so callers can attribute redactions without seeing rule internals.
func (enforcer *LayeredEnforcer) InspectRetrieved(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject, content []byte) ([]byte, domain.GuardrailVerdict, error) {
	return enforcer.inspect(ctx, config, &inspection{stage: domain.GuardrailStageRetrieval, subject: subject, content: content, sizeBytes: len(content)})
}

// OpenOutputSession starts one streaming inspection with buffered lookback for cross-chunk matches.
func (enforcer *LayeredEnforcer) OpenOutputSession(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject) (contract.GuardrailOutputSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if subject.TenantID <= 0 || strings.TrimSpace(subject.RequestID) == "" {
		return nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	release, err := enforcer.compiler.Compile(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := enforcer.checkProviders(release); err != nil {
		return nil, err
	}
	return &outputSession{
		enforcer: enforcer, config: config, subject: subject,
		lookback: max(enforcer.baseline.Lookback, release.Lookback),
	}, nil
}

// TerminalFrame is the payload-free, policy-safe final reply for a blocked stream.
func TerminalFrame(subject domain.GuardrailSubject, route string, sequence int64, now time.Time) domain.TurnReply {
	return domain.TurnReply{
		TenantID: subject.TenantID, RequestID: subject.RequestID, ConversationID: subject.ConversationID,
		ReplyRoute: route, Sequence: sequence, Final: true, ErrorCode: ErrorCodePolicyViolation,
		AgentVersion: subject.ReleaseVersion, EmittedAt: now,
	}
}

type inspection struct {
	stage     domain.GuardrailStage
	subject   domain.GuardrailSubject
	content   []byte
	sizeBytes int
	// from skips matches ending at or before this offset: they were reported by an earlier window.
	from int
	call *domain.ToolCall
}

type outcome struct {
	blocked  []string
	hits     []string
	layer    domain.GuardrailLayer
	action   domain.GuardrailAction
	severity domain.GuardrailSeverity
	redacted int
	pending  bool
}

func (enforcer *LayeredEnforcer) inspect(ctx context.Context, config domain.GuardrailConfig, in *inspection) ([]byte, domain.GuardrailVerdict, error) {
	if err := ctx.Err(); err != nil {
		return nil, domain.GuardrailVerdict{}, err
	}
	release, err := enforcer.compiler.Compile(ctx, config)
	if err != nil {
		return nil, domain.GuardrailVerdict{}, err
	}
	started := enforcer.now()
	result := &outcome{}
	var markers []*compiledRule
	for _, layer := range []*CompiledRuleSet{enforcer.baseline, release} {
		for _, rule := range layer.rules {
			if _, applies := rule.stages[in.stage]; !applies {
				continue
			}
			if rule.rule.Kind == domain.GuardrailKindUntrustedContentMarker {
				markers = append(markers, rule)
				continue
			}
			if err := enforcer.applyRule(ctx, rule, in, result); err != nil {
				return nil, domain.GuardrailVerdict{}, err
			}
		}
	}
	// Fencing runs last so validation and matching rules see the raw content.
	for _, marker := range markers {
		in.content = markUntrusted(in.content, marker.open, marker.close)
	}
	verdict := domain.GuardrailVerdict{Allowed: len(result.blocked) == 0, RuleIDs: result.hits, Severity: result.severity, RedactedBytes: result.redacted, Version: config.Version}
	if err := enforcer.record(ctx, config, in, result, enforcer.now().Sub(started)); err != nil {
		return nil, verdict, err
	}
	if !verdict.Allowed {
		cause := error(domain.ErrForbidden)
		if result.pending {
			cause = ErrApprovalPending
		}
		return nil, verdict, &ViolationError{Stage: in.stage, RuleIDs: result.blocked, Severity: result.severity, PolicyVersion: config.Version, Err: cause}
	}
	return in.content, verdict, nil
}

func (enforcer *LayeredEnforcer) applyRule(ctx context.Context, rule *compiledRule, in *inspection, result *outcome) error {
	spans, hit, err := enforcer.match(ctx, rule, in)
	var approval *approvalOutcome
	if errors.As(err, &approval) {
		hit, err = true, nil
		result.pending = result.pending || approval.pending
	}
	if err != nil {
		return err
	}
	if !hit {
		return nil
	}
	result.hits = append(result.hits, rule.rule.ID)
	if result.layer == "" || rule.layer == domain.GuardrailLayerBaseline {
		result.layer = rule.layer
	}
	if rule.rule.Severity == domain.GuardrailSeverityHard || result.severity == "" {
		result.severity = rule.rule.Severity
	}
	switch rule.rule.Action {
	case domain.GuardrailActionBlock:
		result.blocked = append(result.blocked, rule.rule.ID)
		result.action = domain.GuardrailActionBlock
	case domain.GuardrailActionRedact:
		var redacted int
		in.content, redacted = redact(in.content, spans)
		result.redacted += redacted
		if result.action != domain.GuardrailActionBlock {
			result.action = domain.GuardrailActionRedact
		}
	default:
		if result.action == "" {
			result.action = domain.GuardrailActionFlag
		}
	}
	return nil
}

// match reports whether the rule fires and, for redactable kinds, where.
func (enforcer *LayeredEnforcer) match(ctx context.Context, rule *compiledRule, in *inspection) ([]domain.ContentSpan, bool, error) {
	switch rule.rule.Kind {
	case domain.GuardrailKindMaxInputBytes, domain.GuardrailKindMaxOutputBytes:
		return nil, in.sizeBytes > rule.maxBytes, nil
	case domain.GuardrailKindJSONSchema:
		var value any
		if err := json.Unmarshal(in.content, &value); err != nil {
			return nil, true, nil
		}
		return nil, rule.schema.check(value, "$") != nil, nil
	case domain.GuardrailKindToolAllowlist:
		if in.call == nil {
			return nil, true, nil
		}
		_, allowed := rule.names[in.call.ToolID]
		return nil, !allowed, nil
	case domain.GuardrailKindDestinationAllowlist:
		return nil, !destinationsAllowed(in.content, rule.names), nil
	case domain.GuardrailKindExactPhrase:
		spans := phraseSpans(in.content, rule.phrases, rule.fold, in.from)
		return spans, len(spans) > 0, nil
	case domain.GuardrailKindRegex:
		spans := regexSpans(in.content, rule.pattern, in.from)
		return spans, len(spans) > 0, nil
	case domain.GuardrailKindIrreversibleToolApproval:
		return nil, false, enforcer.requireApproval(ctx, rule, in)
	default:
		return enforcer.classify(ctx, rule, in)
	}
}

func (enforcer *LayeredEnforcer) requireApproval(ctx context.Context, rule *compiledRule, in *inspection) error {
	if in.call == nil {
		return fmt.Errorf("%w: approval rule %q needs a tool call", domain.ErrValidation, rule.rule.ID)
	}
	if len(rule.names) > 0 {
		if _, listed := rule.names[in.call.ToolID]; !listed {
			return nil
		}
	}
	if enforcer.approvals == nil {
		return fmt.Errorf("%w: rule %q needs a tool approval gate", domain.ErrDegraded, rule.rule.ID)
	}
	decision, err := enforcer.approvals.Decide(ctx, *in.call, rule.rule.ID)
	if err != nil {
		return err
	}
	switch decision {
	case domain.ApprovalApproved:
		return nil
	case domain.ApprovalPending:
		return &approvalOutcome{pending: true}
	case domain.ApprovalDenied:
		return &approvalOutcome{}
	}
	return fmt.Errorf("%w: unknown approval decision %q", domain.ErrValidation, decision)
}

// approvalOutcome signals a non-approved decision to applyRule without leaving match's error channel.
type approvalOutcome struct{ pending bool }

func (o *approvalOutcome) Error() string { return "tool approval not granted" }

func (enforcer *LayeredEnforcer) classify(ctx context.Context, rule *compiledRule, in *inspection) ([]domain.ContentSpan, bool, error) {
	provider := enforcer.classifiers[rule.rule.Kind]
	if provider == nil {
		return nil, false, fmt.Errorf("%w: rule %q needs a %s classifier provider", domain.ErrDegraded, rule.rule.ID, rule.rule.Kind)
	}
	classification, err := provider.Classify(ctx, rule.rule.Kind, in.content)
	if err != nil {
		return nil, false, fmt.Errorf("classifier %s: %w", rule.rule.Kind, err)
	}
	if classification.Score < rule.threshold {
		return nil, false, nil
	}
	spans := classification.Spans
	if len(spans) == 0 {
		spans = []domain.ContentSpan{{Start: 0, End: len(in.content)}}
	}
	for _, span := range spans {
		if span.Start < 0 || span.End > len(in.content) || span.Start >= span.End {
			return nil, false, fmt.Errorf("%w: classifier %s returned an invalid span", domain.ErrValidation, rule.rule.Kind)
		}
	}
	return spans, true, nil
}

func (enforcer *LayeredEnforcer) checkProviders(release *CompiledRuleSet) error {
	for kind := range release.classifierKinds {
		if enforcer.classifiers[kind] == nil {
			return fmt.Errorf("%w: policy needs a %s classifier provider", domain.ErrDegraded, kind)
		}
	}
	return nil
}

func (enforcer *LayeredEnforcer) record(ctx context.Context, config domain.GuardrailConfig, in *inspection, result *outcome, duration time.Duration) error {
	if len(result.hits) == 0 {
		return nil
	}
	event := domain.SafetyEvent{
		TenantID: in.subject.TenantID, Stage: in.stage, Layer: result.layer, Action: result.action,
		RuleIDs: append([]string(nil), result.hits...), Severity: result.severity,
		ReleaseVersion: in.subject.ReleaseVersion, PolicyVersion: config.Version,
		Duration: duration, OccurredAt: enforcer.now(),
	}
	if err := enforcer.events.Record(ctx, event); err != nil {
		return fmt.Errorf("safety event: %w", err)
	}
	if enforcer.audit == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	outcome := domain.DecisionAllow
	if event.Action == domain.GuardrailActionBlock {
		outcome = domain.DecisionDeny
	}
	if result.pending {
		outcome = domain.DecisionPending
	}
	record := domain.DecisionRecord{
		TenantID: event.TenantID, Principal: in.subject.Principal, Category: AuditCategory,
		Action: string(event.Stage), Resource: string(event.Action), ReleaseVersion: event.ReleaseVersion,
		PolicyVersion: event.PolicyVersion, Outcome: outcome, Reason: strings.Join(event.RuleIDs, ","),
		RequestID: in.subject.RequestID, ConversationID: in.subject.ConversationID,
		Payload: payload, OccurredAt: event.OccurredAt,
	}
	if err := enforcer.audit.Record(ctx, record); err != nil {
		return fmt.Errorf("guardrail audit: %w", err)
	}
	return nil
}

func phraseSpans(content []byte, phrases [][]byte, fold bool, from int) []domain.ContentSpan {
	haystack := content
	if fold {
		haystack = bytes.ToLower(content)
	}
	var spans []domain.ContentSpan
	for _, phrase := range phrases {
		for offset := 0; offset < len(haystack); {
			index := bytes.Index(haystack[offset:], phrase)
			if index < 0 {
				break
			}
			start := offset + index
			end := start + len(phrase)
			if end > from {
				spans = append(spans, domain.ContentSpan{Start: start, End: end})
			}
			offset = end
		}
	}
	return spans
}

func regexSpans(content []byte, pattern *regexp.Regexp, from int) []domain.ContentSpan {
	var spans []domain.ContentSpan
	for _, loc := range pattern.FindAllIndex(content, -1) {
		if loc[1] > loc[0] && loc[1] > from {
			spans = append(spans, domain.ContentSpan{Start: loc[0], End: loc[1]})
		}
	}
	return spans
}

// redact replaces spans with RedactionMask, merging overlaps, and returns the masked byte count.
func redact(content []byte, spans []domain.ContentSpan) ([]byte, int) {
	if len(spans) == 0 {
		return content, 0
	}
	sorted := append([]domain.ContentSpan(nil), spans...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Start < sorted[j-1].Start; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	out := make([]byte, 0, len(content))
	cursor, redacted := 0, 0
	for _, span := range sorted {
		if span.Start < cursor {
			span.Start = cursor
		}
		if span.End <= span.Start {
			continue
		}
		out = append(out, content[cursor:span.Start]...)
		out = append(out, RedactionMask...)
		redacted += span.End - span.Start
		cursor = span.End
	}
	out = append(out, content[cursor:]...)
	return out, redacted
}

// markUntrusted fences content and neutralizes embedded closing markers so it cannot escape the fence.
func markUntrusted(content, open, closing []byte) []byte {
	body := bytes.ReplaceAll(content, closing, nil)
	out := make([]byte, 0, len(open)+len(body)+len(closing))
	out = append(out, open...)
	out = append(out, body...)
	return append(out, closing...)
}

var urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>\\]+`)

// destinationsAllowed accepts arguments whose every URL host is an allowlisted host or one of its subdomains.
func destinationsAllowed(arguments []byte, hosts map[string]struct{}) bool {
	for _, raw := range urlPattern.FindAll(arguments, -1) {
		parsed, err := url.Parse(string(raw))
		if err != nil || parsed.Hostname() == "" {
			return false
		}
		if !hostAllowed(strings.ToLower(parsed.Hostname()), hosts) {
			return false
		}
	}
	return true
}

func hostAllowed(host string, hosts map[string]struct{}) bool {
	for candidate := host; candidate != ""; {
		if _, ok := hosts[candidate]; ok {
			return true
		}
		dot := strings.IndexByte(candidate, '.')
		if dot < 0 {
			return false
		}
		candidate = candidate[dot+1:]
	}
	return false
}

// IsPending reports whether an error is a pending-approval violation.
func IsPending(err error) bool { return errors.Is(err, ErrApprovalPending) }

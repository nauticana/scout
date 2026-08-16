package guardrail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
)

// RuleSetSchemaVersion is the only envelope version this compiler accepts.
const RuleSetSchemaVersion = 1

// CompilerConfig bounds rule envelopes; zero fields take the documented defaults.
type CompilerConfig struct {
	// MaxRules caps rules per envelope; default 256.
	MaxRules int
	// MaxRulesBytes caps the raw envelope; default 256 KiB.
	MaxRulesBytes int
	// MaxPatternBytes caps one phrase or regex source; default 512.
	MaxPatternBytes int
	// MaxLookbackBytes caps regex max_match_bytes and therefore streaming hold; default 4096.
	MaxLookbackBytes int
	// MaxListEntries caps allowlist and phrase list sizes; default 1024.
	MaxListEntries int
	// CacheEntries bounds compiled envelopes retained by digest; default 256.
	CacheEntries int
	Now          func() time.Time
}

func (config CompilerConfig) withDefaults() CompilerConfig {
	defaults := CompilerConfig{MaxRules: 256, MaxRulesBytes: 256 << 10, MaxPatternBytes: 512, MaxLookbackBytes: 4096, MaxListEntries: 1024, CacheEntries: 256}
	if config.MaxRules == 0 {
		config.MaxRules = defaults.MaxRules
	}
	if config.MaxRulesBytes == 0 {
		config.MaxRulesBytes = defaults.MaxRulesBytes
	}
	if config.MaxPatternBytes == 0 {
		config.MaxPatternBytes = defaults.MaxPatternBytes
	}
	if config.MaxLookbackBytes == 0 {
		config.MaxLookbackBytes = defaults.MaxLookbackBytes
	}
	if config.MaxListEntries == 0 {
		config.MaxListEntries = defaults.MaxListEntries
	}
	if config.CacheEntries == 0 {
		config.CacheEntries = defaults.CacheEntries
	}
	return config
}

// RuleSetCompiler validates envelopes at publication and compiles them once per digest at runtime.
type RuleSetCompiler struct {
	config CompilerConfig
	mu     sync.Mutex
	cache  *lru.Cache[string, *CompiledRuleSet]
}

var _ contract.GuardrailRuleCompiler = (*RuleSetCompiler)(nil)

// NewRuleSetCompiler validates the bounds and returns a compiler with a bounded digest cache.
func NewRuleSetCompiler(config CompilerConfig) (*RuleSetCompiler, error) {
	config = config.withDefaults()
	if config.MaxRules < 0 || config.MaxRulesBytes < 0 || config.MaxPatternBytes < 0 || config.MaxLookbackBytes < 0 || config.MaxListEntries < 0 || config.CacheEntries < 0 {
		return nil, fmt.Errorf("guardrail compiler: bounds cannot be negative")
	}
	if config.MaxLookbackBytes < config.MaxPatternBytes {
		return nil, fmt.Errorf("guardrail compiler: max lookback must cover the longest phrase")
	}
	return &RuleSetCompiler{config: config, cache: lru.New[string, *CompiledRuleSet](config.CacheEntries, config.Now)}, nil
}

// Validate parses the envelope, verifies its digest, and compiles every rule without caching.
func (compiler *RuleSetCompiler) Validate(ctx context.Context, config domain.GuardrailConfig) (domain.GuardrailRuleSet, error) {
	if err := ctx.Err(); err != nil {
		return domain.GuardrailRuleSet{}, err
	}
	compiled, err := compiler.compile(config, domain.GuardrailLayerRelease)
	if err != nil {
		return domain.GuardrailRuleSet{}, err
	}
	return compiled.RuleSet, nil
}

// Compile returns the cached compiled envelope for the config digest, compiling on first use.
func (compiler *RuleSetCompiler) Compile(ctx context.Context, config domain.GuardrailConfig) (*CompiledRuleSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest, err := verifyDigest(config)
	if err != nil {
		return nil, err
	}
	key := config.Version + "/" + digest
	compiler.mu.Lock()
	defer compiler.mu.Unlock()
	if cached, ok := compiler.cache.Get(key); ok {
		return cached, nil
	}
	compiled, err := compiler.compile(config, domain.GuardrailLayerRelease)
	if err != nil {
		return nil, err
	}
	compiler.cache.Set(key, compiled, 0)
	return compiled, nil
}

// CompileBaseline compiles the operator-owned, release-independent rule set.
func (compiler *RuleSetCompiler) CompileBaseline(set domain.GuardrailRuleSet) (*CompiledRuleSet, error) {
	return compiler.compileSet(set, domain.GuardrailLayerBaseline, "", "baseline")
}

func (compiler *RuleSetCompiler) compile(config domain.GuardrailConfig, layer domain.GuardrailLayer) (*CompiledRuleSet, error) {
	digest, err := verifyDigest(config)
	if err != nil {
		return nil, err
	}
	if len(config.Rules) > compiler.config.MaxRulesBytes {
		return nil, fmt.Errorf("%w: guardrail rules exceed %d bytes", domain.ErrValidation, compiler.config.MaxRulesBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(config.Rules))
	decoder.DisallowUnknownFields()
	var set domain.GuardrailRuleSet
	if err := decoder.Decode(&set); err != nil {
		return nil, fmt.Errorf("%w: guardrail rules are not a valid envelope: %v", domain.ErrValidation, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: guardrail rules contain trailing data", domain.ErrValidation)
	}
	return compiler.compileSet(set, layer, digest, config.Version)
}

func (compiler *RuleSetCompiler) compileSet(set domain.GuardrailRuleSet, layer domain.GuardrailLayer, digest, version string) (*CompiledRuleSet, error) {
	if set.SchemaVersion != RuleSetSchemaVersion {
		return nil, fmt.Errorf("%w: unsupported guardrail schema version %d", domain.ErrValidation, set.SchemaVersion)
	}
	if len(set.Rules) > compiler.config.MaxRules {
		return nil, fmt.Errorf("%w: guardrail envelope exceeds %d rules", domain.ErrValidation, compiler.config.MaxRules)
	}
	compiled := &CompiledRuleSet{Digest: digest, Version: version, Layer: layer, RuleSet: set, classifierKinds: map[domain.GuardrailRuleKind]struct{}{}}
	seen := make(map[string]struct{}, len(set.Rules))
	for i, rule := range set.Rules {
		if _, duplicate := seen[rule.ID]; duplicate || strings.TrimSpace(rule.ID) == "" {
			return nil, fmt.Errorf("%w: rule %d needs a unique id", domain.ErrValidation, i)
		}
		seen[rule.ID] = struct{}{}
		item, err := compiler.compileRule(rule, layer)
		if err != nil {
			return nil, fmt.Errorf("%w: rule %q: %v", domain.ErrValidation, rule.ID, err)
		}
		compiled.rules = append(compiled.rules, item)
		if _, output := item.stages[domain.GuardrailStageOutput]; output && item.lookback > compiled.Lookback {
			compiled.Lookback = item.lookback
		}
		if item.kindSpec.classifier {
			compiled.classifierKinds[rule.Kind] = struct{}{}
		}
	}
	return compiled, nil
}

func (compiler *RuleSetCompiler) compileRule(rule domain.GuardrailRule, layer domain.GuardrailLayer) (*compiledRule, error) {
	spec, known := kindSpecs[rule.Kind]
	if !known {
		return nil, fmt.Errorf("unknown kind %q", rule.Kind)
	}
	if _, ok := spec.actions[rule.Action]; !ok {
		return nil, fmt.Errorf("action %q is not allowed for kind %s", rule.Action, rule.Kind)
	}
	if rule.Severity != domain.GuardrailSeverityHard && rule.Severity != domain.GuardrailSeveritySoft {
		return nil, fmt.Errorf("severity must be hard or soft")
	}
	item := &compiledRule{rule: rule, layer: layer, kindSpec: spec, stages: map[domain.GuardrailStage]struct{}{}}
	stages := rule.Stages
	if len(stages) == 0 {
		stages = spec.defaultStages
	}
	for _, stage := range stages {
		if _, ok := spec.stages[stage]; !ok {
			return nil, fmt.Errorf("stage %q is not allowed for kind %s", stage, rule.Kind)
		}
		item.stages[stage] = struct{}{}
	}
	if err := compiler.compileParams(item); err != nil {
		return nil, err
	}
	return item, nil
}

func (compiler *RuleSetCompiler) compileParams(item *compiledRule) error {
	rule := item.rule
	var params struct {
		Max             *int        `json:"max"`
		Schema          *jsonSchema `json:"schema"`
		Tools           []string    `json:"tools"`
		Hosts           []string    `json:"hosts"`
		Phrases         []string    `json:"phrases"`
		CaseInsensitive bool        `json:"case_insensitive"`
		Pattern         string      `json:"pattern"`
		MaxMatchBytes   int         `json:"max_match_bytes"`
		Open            string      `json:"open"`
		Close           string      `json:"close"`
		Threshold       *float64    `json:"threshold"`
	}
	if len(rule.Params) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(rule.Params))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&params); err != nil {
			return fmt.Errorf("invalid params: %v", err)
		}
	}
	bounded := func(list []string, name string) error {
		if len(list) > compiler.config.MaxListEntries {
			return fmt.Errorf("%s exceeds %d entries", name, compiler.config.MaxListEntries)
		}
		for _, entry := range list {
			if strings.TrimSpace(entry) == "" || len(entry) > compiler.config.MaxPatternBytes {
				return fmt.Errorf("%s entries must be non-empty and at most %d bytes", name, compiler.config.MaxPatternBytes)
			}
		}
		return nil
	}
	switch rule.Kind {
	case domain.GuardrailKindMaxInputBytes, domain.GuardrailKindMaxOutputBytes:
		if params.Max == nil || *params.Max <= 0 {
			return fmt.Errorf("max must be positive")
		}
		item.maxBytes = *params.Max
	case domain.GuardrailKindJSONSchema:
		if params.Schema == nil {
			return fmt.Errorf("schema is required")
		}
		if err := params.Schema.validate(0); err != nil {
			return err
		}
		item.schema = params.Schema
	case domain.GuardrailKindToolAllowlist:
		if len(params.Tools) == 0 {
			return fmt.Errorf("tools must not be empty")
		}
		if err := bounded(params.Tools, "tools"); err != nil {
			return err
		}
		item.names = toSet(params.Tools, false)
	case domain.GuardrailKindDestinationAllowlist:
		if len(params.Hosts) == 0 {
			return fmt.Errorf("hosts must not be empty")
		}
		if err := bounded(params.Hosts, "hosts"); err != nil {
			return err
		}
		item.names = toSet(params.Hosts, true)
	case domain.GuardrailKindExactPhrase:
		if len(params.Phrases) == 0 {
			return fmt.Errorf("phrases must not be empty")
		}
		if err := bounded(params.Phrases, "phrases"); err != nil {
			return err
		}
		item.fold = params.CaseInsensitive
		for _, phrase := range params.Phrases {
			if item.fold {
				phrase = strings.ToLower(phrase)
			}
			item.phrases = append(item.phrases, []byte(phrase))
			item.lookback = max(item.lookback, len(phrase))
		}
	case domain.GuardrailKindRegex:
		if params.Pattern == "" || len(params.Pattern) > compiler.config.MaxPatternBytes {
			return fmt.Errorf("pattern must be non-empty and at most %d bytes", compiler.config.MaxPatternBytes)
		}
		if params.MaxMatchBytes <= 0 || params.MaxMatchBytes > compiler.config.MaxLookbackBytes {
			return fmt.Errorf("max_match_bytes must be within (0, %d]", compiler.config.MaxLookbackBytes)
		}
		pattern, err := regexp.Compile(params.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern: %v", err)
		}
		if _, complete := pattern.LiteralPrefix(); !complete && pattern.NumSubexp() > 8 {
			return fmt.Errorf("pattern exceeds 8 capture groups")
		}
		item.pattern = pattern
		item.lookback = params.MaxMatchBytes
	case domain.GuardrailKindUntrustedContentMarker:
		item.open, item.close = []byte(params.Open), []byte(params.Close)
		if len(item.open) == 0 {
			item.open = []byte(DefaultUntrustedOpen)
		}
		if len(item.close) == 0 {
			item.close = []byte(DefaultUntrustedClose)
		}
		if len(item.open) > compiler.config.MaxPatternBytes || len(item.close) > compiler.config.MaxPatternBytes {
			return fmt.Errorf("markers exceed %d bytes", compiler.config.MaxPatternBytes)
		}
	case domain.GuardrailKindIrreversibleToolApproval:
		if err := bounded(params.Tools, "tools"); err != nil {
			return err
		}
		item.names = toSet(params.Tools, false)
	default:
		threshold := 0.5
		if params.Threshold != nil {
			threshold = *params.Threshold
		}
		if threshold <= 0 || threshold > 1 {
			return fmt.Errorf("threshold must be within (0, 1]")
		}
		item.threshold = threshold
	}
	return nil
}

func verifyDigest(config domain.GuardrailConfig) (string, error) {
	if strings.TrimSpace(config.Version) == "" {
		return "", fmt.Errorf("%w: guardrail version is required", domain.ErrValidation)
	}
	sum := sha256.Sum256(config.Rules)
	digest := hex.EncodeToString(sum[:])
	if !strings.EqualFold(digest, config.RulesDigest) {
		return "", fmt.Errorf("%w: guardrail rules digest does not match content", domain.ErrValidation)
	}
	return digest, nil
}

// Digest returns the SHA-256 hex digest publishers must store beside a rule envelope.
func Digest(rules []byte) string {
	sum := sha256.Sum256(rules)
	return hex.EncodeToString(sum[:])
}

func toSet(values []string, fold bool) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if fold {
			value = strings.ToLower(value)
		}
		set[strings.TrimSpace(value)] = struct{}{}
	}
	return set
}

// CompiledRuleSet is one immutable, executable rule envelope.
type CompiledRuleSet struct {
	Digest  string
	Version string
	Layer   domain.GuardrailLayer
	RuleSet domain.GuardrailRuleSet
	// Lookback is the largest phrase or regex match an output session must hold to catch cross-chunk matches.
	Lookback        int
	rules           []*compiledRule
	classifierKinds map[domain.GuardrailRuleKind]struct{}
}

type compiledRule struct {
	rule      domain.GuardrailRule
	layer     domain.GuardrailLayer
	kindSpec  kindSpec
	stages    map[domain.GuardrailStage]struct{}
	maxBytes  int
	schema    *jsonSchema
	names     map[string]struct{}
	phrases   [][]byte
	fold      bool
	pattern   *regexp.Regexp
	lookback  int
	open      []byte
	close     []byte
	threshold float64
}

type kindSpec struct {
	stages        map[domain.GuardrailStage]struct{}
	defaultStages []domain.GuardrailStage
	actions       map[domain.GuardrailAction]struct{}
	classifier    bool
}

var (
	allStages       = []domain.GuardrailStage{domain.GuardrailStageInput, domain.GuardrailStageOutput, domain.GuardrailStageToolInput, domain.GuardrailStageToolOutput, domain.GuardrailStageRetrieval}
	blockOrFlag     = actionSet(domain.GuardrailActionBlock, domain.GuardrailActionFlag)
	everyAction     = actionSet(domain.GuardrailActionBlock, domain.GuardrailActionRedact, domain.GuardrailActionFlag)
	contentKindSpec = kindSpec{stages: stageSet(allStages...), defaultStages: allStages, actions: everyAction}
	classifierSpec  = kindSpec{stages: stageSet(allStages...), defaultStages: allStages, actions: everyAction, classifier: true}
	kindSpecs       = map[domain.GuardrailRuleKind]kindSpec{
		domain.GuardrailKindMaxInputBytes:            {stages: stageSet(domain.GuardrailStageInput, domain.GuardrailStageToolInput, domain.GuardrailStageRetrieval), defaultStages: []domain.GuardrailStage{domain.GuardrailStageInput}, actions: blockOrFlag},
		domain.GuardrailKindMaxOutputBytes:           {stages: stageSet(domain.GuardrailStageOutput, domain.GuardrailStageToolOutput), defaultStages: []domain.GuardrailStage{domain.GuardrailStageOutput}, actions: blockOrFlag},
		domain.GuardrailKindJSONSchema:               {stages: stageSet(domain.GuardrailStageToolInput, domain.GuardrailStageToolOutput), defaultStages: []domain.GuardrailStage{domain.GuardrailStageToolOutput}, actions: blockOrFlag},
		domain.GuardrailKindToolAllowlist:            {stages: stageSet(domain.GuardrailStageToolInput), defaultStages: []domain.GuardrailStage{domain.GuardrailStageToolInput}, actions: blockOrFlag},
		domain.GuardrailKindDestinationAllowlist:     {stages: stageSet(domain.GuardrailStageToolInput), defaultStages: []domain.GuardrailStage{domain.GuardrailStageToolInput}, actions: blockOrFlag},
		domain.GuardrailKindExactPhrase:              contentKindSpec,
		domain.GuardrailKindRegex:                    contentKindSpec,
		domain.GuardrailKindUntrustedContentMarker:   {stages: stageSet(domain.GuardrailStageToolOutput, domain.GuardrailStageRetrieval), defaultStages: []domain.GuardrailStage{domain.GuardrailStageToolOutput, domain.GuardrailStageRetrieval}, actions: actionSet(domain.GuardrailActionRedact)},
		domain.GuardrailKindIrreversibleToolApproval: {stages: stageSet(domain.GuardrailStageToolInput), defaultStages: []domain.GuardrailStage{domain.GuardrailStageToolInput}, actions: actionSet(domain.GuardrailActionBlock)},
		domain.GuardrailKindPII:                      classifierSpec,
		domain.GuardrailKindToxicity:                 classifierSpec,
		domain.GuardrailKindMalware:                  classifierSpec,
		domain.GuardrailKindPromptInjection:          classifierSpec,
		domain.GuardrailKindJailbreak:                classifierSpec,
	}
)

func stageSet(stages ...domain.GuardrailStage) map[domain.GuardrailStage]struct{} {
	set := make(map[domain.GuardrailStage]struct{}, len(stages))
	for _, stage := range stages {
		set[stage] = struct{}{}
	}
	return set
}

func actionSet(actions ...domain.GuardrailAction) map[domain.GuardrailAction]struct{} {
	set := make(map[domain.GuardrailAction]struct{}, len(actions))
	for _, action := range actions {
		set[action] = struct{}{}
	}
	return set
}

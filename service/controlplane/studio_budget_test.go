package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

const (
	eurMonthBudget = `{"tokens":0,"cost_minor_units":5000,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`
	tokenDayBudget = `{"tokens":9000,"cost_minor_units":0,"period":"day","timezone":"UTC"}`
)

func budgetChange(value string) domain.AgentSettingChange {
	return domain.AgentSettingChange{
		AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceBudget, ResourceID: "budget",
		Value: json.RawMessage(value), ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
	}
}

func fieldMessage(err error) string {
	var invalid *StudioValidationError
	if !errors.As(err, &invalid) || len(invalid.Fields) == 0 {
		return ""
	}
	return invalid.Fields[0].Field + ": " + invalid.Fields[0].Message
}

func eurPricer(currency string, err error) *fake.ModelPricerFunc {
	pricer := fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 0, currency, err
	})
	return &pricer
}

// eurCatalog holds only EUR at two decimals.
func eurCatalog() *fake.CurrencyCatalog {
	return &fake.CurrencyCatalog{Exponents: map[string]int{"EUR": 2}}
}

func TestSaveSettingChecksABudgetsUnitAgainstTheCatalog(t *testing.T) {
	for name, tc := range map[string]struct {
		catalog map[string]int
		value   string
		want    string
	}{
		"unknown currency":  {catalog: nil, value: eurMonthBudget, want: "value: currency EUR is not in the currency catalog"},
		"exponent mismatch": {catalog: map[string]int{"EUR": 3}, value: eurMonthBudget, want: "value: currency EUR has exponent 3, not 2"},
		"no period":         {catalog: map[string]int{"EUR": 2}, value: `{"tokens":10,"cost_minor_units":0}`, want: "value: validation failed: a budget must name its period"},
	} {
		service, query := settingsService(nil)
		service.Currencies = &fake.CurrencyCatalog{Exponents: tc.catalog}
		_, err := service.SaveSetting(context.Background(), humanActor(8, 9), budgetChange(tc.value))
		if !errors.Is(err, domain.ErrValidation) || !strings.HasPrefix(fieldMessage(err), tc.want) {
			t.Errorf("%s: error = %v (%q)", name, err, fieldMessage(err))
		}
		if slices.Contains(query.queries, qStudioInsertSetting) || slices.Contains(query.queries, qStudioBumpDraft) {
			t.Errorf("%s: a refused budget wrote %v", name, query.queries)
		}
	}
	service, query := settingsService(nil)
	catalog := eurCatalog()
	service.Currencies = catalog
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), budgetChange(eurMonthBudget)); err != nil {
		t.Fatalf("SaveSetting: %v", err)
	}
	if args := query.args[qStudioInsertSetting]; args[7] != eurMonthBudget {
		t.Fatalf("insert = %v", args)
	}
	if len(catalog.Reads) != 1 || catalog.Reads[0] != "EUR" {
		t.Fatalf("catalog read = %v", catalog.Reads)
	}
}

func budgetPublishService(value string) (*StudioService, *studioQueryFake) {
	service, query := publishService()
	service.Scopes = studioScopes([]domain.ScopedBinding{setting("a:writer-a", domain.ResourceBudget, value)})
	service.Currencies = eurCatalog()
	return service, query
}

func TestPublishRefusesACostBudgetTheRuntimeCouldNotEnforce(t *testing.T) {
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	for name, tc := range map[string]struct {
		catalog map[string]int
		pricer  *fake.ModelPricerFunc
		want    error
	}{
		"exponent changed after save": {catalog: map[string]int{"EUR": 3}, pricer: eurPricer("EUR", nil), want: domain.ErrValidation},
		"unpriced model":              {pricer: eurPricer("", domain.ErrNotFound), want: domain.ErrValidation},
		"model priced elsewhere":      {pricer: eurPricer("USD", nil), want: domain.ErrValidation},
		"no pricer":                   {want: domain.ErrNotReady},
	} {
		service, query := budgetPublishService(eurMonthBudget)
		if tc.catalog != nil {
			service.Currencies = &fake.CurrencyCatalog{Exponents: tc.catalog}
		}
		if tc.pricer != nil {
			service.Pricer = *tc.pricer
		}
		if _, err := service.Publish(context.Background(), humanActor(8, 9), request); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
		if slices.Contains(query.queries, qStudioInsertVersion) {
			t.Errorf("%s: a refused release was written", name)
		}
	}
	service, query := budgetPublishService(eurMonthBudget)
	var priced []domain.ModelReference
	service.Pricer = fake.ModelPricerFunc(func(_ context.Context, reference domain.ModelReference, _ domain.ModelUsage) (int64, string, error) {
		priced = append(priced, reference)
		return 0, "EUR", nil
	})
	if _, err := service.Publish(context.Background(), humanActor(8, 9), request); err != nil {
		t.Fatalf("priced publish: %v", err)
	}
	if len(priced) != 1 || priced[0] != (domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}) || !slices.Contains(query.queries, qStudioInsertVersion) {
		t.Fatalf("priced %v, queries %v", priced, query.queries)
	}
	tokensOnly, _ := budgetPublishService(tokenDayBudget)
	if _, err := tokensOnly.Publish(context.Background(), humanActor(8, 9), request); err != nil {
		t.Fatalf("a tokens-only budget needs no pricer: %v", err)
	}
	if reads := tokensOnly.Currencies.(*fake.CurrencyCatalog).Reads; len(reads) != 0 {
		t.Fatalf("a tokens-only budget read the currency catalog: %v", reads)
	}
}

func TestRestoreRefusesAVersionWhoseBudgetUnitDrifted(t *testing.T) {
	service, query := publishService()
	service.Pricer = *eurPricer("EUR", nil)
	frozenBudget, err := json.Marshal([]domain.EffectiveResource{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(eurMonthBudget)}})
	if err != nil {
		t.Fatal(err)
	}
	query.rows[qStudioGetVersion] = [][]any{{`{"agent_id":"writer-a","version":"1","definition_digest":"d","models":{"text":{"provider_id":"provider-a","model_id":"model-a"}}}`}}
	query.rows[qStudioGetSettings] = [][]any{{"a:writer-a", string(frozenBudget), scheduledAt, int64(9)}}
	service.Currencies = &fake.CurrencyCatalog{Exponents: map[string]int{"EUR": 3}}
	_, err = service.Restore(context.Background(), humanActor(8, 9), domain.AgentRestoreRequest{AgentID: "writer-a", Version: "1"})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(fieldMessage(err), "re-save the budget") {
		t.Fatalf("error = %v (%q), want the drifted unit refused", err, fieldMessage(err))
	}
	if slices.Contains(query.queries, qStudioInsertVersion) || query.commits != 0 {
		t.Fatalf("a refused restore wrote %v", query.queries)
	}
}

func TestActivationRefusesAReleaseWhoseBudgetUnitDrifted(t *testing.T) {
	published := []domain.EffectiveResource{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(eurMonthBudget)}}
	definition, digest := frozenDefinitionWithSettings(t, scope.SettingsDigest(published))
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, string(domain.ActivationPending), 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qStudioGetSettings, [][]any{settingsRow(t, published)})
	db.script(qActivationRefuse, [][]any{{int64(41)}})
	activations := activationService(db)
	activations.Studio.Pricer = *eurPricer("EUR", nil)
	activations.Studio.Currencies = &fake.CurrencyCatalog{Exponents: map[string]int{"EUR": 3}}
	report, err := activations.ActivateDue(context.Background(), activationSettings())
	if err != nil || report.Refused != 1 || db.called(qActivationStable) {
		t.Fatalf("report = %+v, error = %v, queries = %v", report, err, db.names())
	}
	if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationGuardRefused) {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestAgentPublisherChecksACostBudgetsUnit(t *testing.T) {
	budgeted := func(publisher *AgentPublisher) *AgentPublisher {
		frozen(publisher)
		publisher.Compiler = fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) { return validGraph(), nil })
		publisher.Store = fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error { return nil })
		publisher.Effective = fake.EffectiveConfigCompilerFunc(func(_ context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error) {
			return domain.EffectiveRelease{TenantID: request.TenantID, AgentID: request.AgentID, AgentVersion: request.AgentVersion,
				Resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(eurMonthBudget)}}}, nil
		})
		return publisher
	}
	stored := 0
	sink := effectiveReleaseSink(func(domain.EffectiveRelease) error { stored++; return nil })
	noDB := budgeted(&AgentPublisher{})
	noDB.Releases = sink
	if err := noDB.Publish(context.Background(), 7, validDefinition()); err == nil || !strings.Contains(err.Error(), "database is required") {
		t.Fatalf("no catalog = %v", err)
	}
	unknown := budgeted(&AgentPublisher{Currencies: &fake.CurrencyCatalog{}})
	unknown.Releases = sink
	if err := unknown.Publish(context.Background(), 7, validDefinition()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown currency = %v", err)
	}
	known := budgeted(&AgentPublisher{Currencies: eurCatalog()})
	known.Releases = sink
	if err := known.Publish(context.Background(), 7, validDefinition()); err != nil {
		t.Fatalf("known currency: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored %d effective releases, want only the checked one", stored)
	}
}

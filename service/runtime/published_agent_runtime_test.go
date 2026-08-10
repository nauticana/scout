package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

func TestPublishedAgentRuntimeBindsDisabledReleaseThroughLiveResolver(t *testing.T) {
	text := domain.ModelReference{ProviderID: "provider", ModelID: "text"}
	image := domain.ModelReference{ProviderID: "provider", ModelID: "image"}
	video := domain.ModelReference{ProviderID: "provider", ModelID: "video"}
	definitionResolver := &publishedResolverStub{definition: domain.AgentDefinition{
		AgentID:          "writer",
		Version:          "7",
		Enabled:          false,
		DefinitionDigest: "digest",
		Models: domain.AgentModelSelection{
			Text:  &text,
			Image: &image,
			Video: &video,
		},
		Languages: []domain.CompiledPrompt{{
			LanguageCode: "en-US",
			Digest:       "prompt-digest",
			Sections:     []domain.CompiledPromptSection{{Caption: "Voice", Instruction: "Direct"}},
		}},
	}}
	provider := &modelProviderRecorder{}
	media := &mediaProviderRecorder{}
	providerFactory := &providerFactoryStub{provider: provider, media: media}
	resolver := &PublishedAgentRuntime{
		Definitions:     definitionResolver,
		Providers:       providerFactory,
		MaxOutputTokens: 512,
	}

	resolved, err := resolver.Resolve(context.Background(), 42, "writer", "de-DE", "conversation")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if definitionResolver.languageCode != "" || definitionResolver.conversationID != "conversation" {
		t.Fatalf("definition resolver arguments: language=%q conversation=%q", definitionResolver.languageCode, definitionResolver.conversationID)
	}
	if resolved.Release() != (domain.AgentReleaseReference{AgentID: "writer", Version: "7", Digest: "digest"}) {
		t.Fatalf("release = %+v", resolved.Release())
	}
	if resolved.Language().LanguageCode != "en-US" {
		t.Fatalf("language = %+v", resolved.Language())
	}
	if resolved.Text() == nil || resolved.Image() == nil || resolved.Video() == nil || len(providerFactory.references) != 3 {
		t.Fatalf("resolved executors or references missing: %+v", providerFactory.references)
	}
	if _, err = resolved.Text().Generate(context.Background(), domain.AgentTask{Task: "Write"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(provider.request.Prompt), "Task: Write") {
		t.Fatalf("default renderer prompt = %q", provider.request.Prompt)
	}
}

func TestPublishedAgentRuntimeRequiresTextAndMediaCapabilities(t *testing.T) {
	base := domain.AgentDefinition{
		AgentID:          "writer",
		Version:          "1",
		DefinitionDigest: "digest",
		Languages:        []domain.CompiledPrompt{{LanguageCode: "en-US"}},
	}
	resolver := &PublishedAgentRuntime{
		Definitions:     &publishedResolverStub{definition: base},
		Providers:       &providerFactoryStub{provider: &modelProviderRecorder{}},
		MaxOutputTokens: 100,
	}
	if _, err := resolver.Resolve(context.Background(), 1, "writer", "", ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("missing text error = %v", err)
	}

	text := domain.ModelReference{ProviderID: "provider", ModelID: "text"}
	image := domain.ModelReference{ProviderID: "provider", ModelID: "image"}
	base.Models = domain.AgentModelSelection{Text: &text, Image: &image}
	resolver.Definitions = &publishedResolverStub{definition: base}
	if _, err := resolver.Resolve(context.Background(), 1, "writer", "", ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("missing media error = %v", err)
	}
}

func TestSelectLanguageExactFallbackAndMissing(t *testing.T) {
	definition := domain.AgentDefinition{
		AgentID: "writer",
		Languages: []domain.CompiledPrompt{
			{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{Instruction: "English"}}},
			{LanguageCode: "fr-FR", Sections: []domain.CompiledPromptSection{{Instruction: "French"}}},
		},
	}
	exact, err := SelectLanguage(definition, "fr-FR", "en-US")
	if err != nil || exact.LanguageCode != "fr-FR" {
		t.Fatalf("exact = %+v, %v", exact, err)
	}
	fallback, err := SelectLanguage(definition, "de-DE", "en-US")
	if err != nil || fallback.LanguageCode != "en-US" {
		t.Fatalf("fallback = %+v, %v", fallback, err)
	}
	fallback.Sections[0].Instruction = "mutated"
	if definition.Languages[0].Sections[0].Instruction != "English" {
		t.Fatal("SelectLanguage exposed mutable definition state")
	}
	if _, err := SelectLanguage(definition, "de-DE", "es-ES"); !errors.Is(err, domain.ErrNoPrompts) {
		t.Fatalf("missing language error = %v", err)
	}
}

type publishedResolverStub struct {
	definition     domain.AgentDefinition
	err            error
	languageCode   string
	conversationID string
}

func (resolver *publishedResolverStub) Resolve(_ context.Context, _ int64, _, languageCode, conversationID string) (domain.AgentDefinition, error) {
	resolver.languageCode = languageCode
	resolver.conversationID = conversationID
	return resolver.definition, resolver.err
}

type providerFactoryStub struct {
	provider   contract.ModelProvider
	media      contract.MediaProvider
	err        error
	references []domain.ModelReference
}

func (factory *providerFactoryStub) Build(_ context.Context, reference domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	factory.references = append(factory.references, reference)
	return factory.provider, factory.media, factory.err
}

var _ contract.PublishedAgentResolver = (*publishedResolverStub)(nil)
var _ contract.AgentProviderFactory = (*providerFactoryStub)(nil)

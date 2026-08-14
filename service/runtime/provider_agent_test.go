package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

func TestProviderAgentExecutesTextAndMedia(t *testing.T) {
	renderer := &rendererRecorder{rendered: "rendered prompt"}
	provider := &modelProviderRecorder{result: domain.ModelResult{
		Output: []byte("generated"),
		Usage:  domain.Usage{InputTokens: 7, OutputTokens: 3},
	}}
	media := &mediaProviderRecorder{}
	sections := []domain.CompiledPromptSection{{Caption: "Voice", Instruction: "Direct"}}
	agent, err := NewProviderAgent(
		"writer",
		domain.ModelReference{ProviderID: "provider", ModelID: "model"},
		sections,
		900,
		renderer,
		provider,
		media,
	)
	if err != nil {
		t.Fatalf("NewProviderAgent: %v", err)
	}
	sections[0].Instruction = "mutated"

	result, err := agent.Generate(context.Background(), domain.AgentTask{Task: "Write"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(result.Output) != "generated" || renderer.agentID != "writer" || renderer.sections[0].Instruction != "Direct" {
		t.Fatalf("text binding was not preserved: result=%q renderer=%+v", result.Output, renderer)
	}
	if provider.selection.Provider != "provider" || provider.selection.Model != "model" {
		t.Fatalf("selection = %+v", provider.selection)
	}
	if string(provider.request.Prompt) != "rendered prompt" || provider.request.MaxOutputTokens != 900 {
		t.Fatalf("request = %+v", provider.request)
	}

	if _, err := agent.GenerateImage(context.Background(), "image prompt", domain.ImageRequest{Prompt: "stale"}); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if media.imageModel != "model" || media.imageRequest.Prompt != "image prompt" {
		t.Fatalf("image request = model %q, %+v", media.imageModel, media.imageRequest)
	}
	if _, err := agent.GenerateVideo(context.Background(), "video prompt", domain.VideoRequest{Prompt: "stale"}); err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if media.videoModel != "model" || media.videoRequest.Prompt != "video prompt" {
		t.Fatalf("video request = model %q, %+v", media.videoModel, media.videoRequest)
	}

	returnedSections := agent.PromptSections()
	returnedSections[0].Instruction = "changed"
	if agent.PromptSections()[0].Instruction != "Direct" {
		t.Fatal("PromptSections exposed mutable runtime state")
	}
}

func TestProviderAgentRejectsUnsupportedExecution(t *testing.T) {
	provider := &modelProviderRecorder{}
	agent, err := NewProviderAgent(
		"writer",
		domain.ModelReference{ProviderID: "provider", ModelID: "model"},
		nil,
		100,
		&rendererRecorder{},
		provider,
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderAgent: %v", err)
	}
	if _, err := agent.GenerateImage(context.Background(), "prompt", domain.ImageRequest{}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("unsupported image error = %v", err)
	}
	if _, err := NewProviderAgent("", domain.ModelReference{}, nil, 0, nil, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid binding error = %v", err)
	}
	if _, err := NewTenantProviderAgent("writer", domain.ModelReference{ProviderID: "provider", ModelID: "model"}, nil, 100,
		&rendererRecorder{}, provider, nil, domain.TenantContext{}, "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing tenant error = %v", err)
	}
}

type rendererRecorder struct {
	rendered string
	agentID  string
	sections []domain.CompiledPromptSection
	task     domain.AgentTask
}

func (renderer *rendererRecorder) Render(agentID string, sections []domain.CompiledPromptSection, task domain.AgentTask) string {
	renderer.agentID = agentID
	renderer.sections = append([]domain.CompiledPromptSection(nil), sections...)
	renderer.task = task
	return renderer.rendered
}

type modelProviderRecorder struct {
	selection domain.ModelSelection
	request   domain.ModelRequest
	result    domain.ModelResult
	err       error
}

func (provider *modelProviderRecorder) Generate(_ context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	provider.selection = selection
	provider.request = request
	return provider.result, provider.err
}

func (*modelProviderRecorder) Stream(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
	return nil, nil
}

type mediaProviderRecorder struct {
	imageModel   string
	imageRequest domain.ImageRequest
	videoModel   string
	videoRequest domain.VideoRequest
}

func (provider *mediaProviderRecorder) GenerateImage(_ context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	provider.imageModel = model
	provider.imageRequest = request
	return []domain.GeneratedMedia{{MimeType: "image/png"}}, nil
}

func (provider *mediaProviderRecorder) GenerateVideo(_ context.Context, model string, request domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	provider.videoModel = model
	provider.videoRequest = request
	return []domain.GeneratedMedia{{MimeType: "video/mp4"}}, nil
}

var _ contract.PromptRenderer = (*rendererRecorder)(nil)
var _ contract.ModelProvider = (*modelProviderRecorder)(nil)
var _ contract.MediaProvider = (*mediaProviderRecorder)(nil)

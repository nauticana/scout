package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

type stubExecutor struct {
	sections     []domain.CompiledPromptSection
	result       domain.ModelResult
	images       []domain.GeneratedMedia
	videos       []domain.GeneratedMedia
	err          error
	imageRequest domain.ImageRequest
	videoRequest domain.VideoRequest
	task         domain.AgentTask
}

func (s *stubExecutor) Generate(_ context.Context, task domain.AgentTask) (domain.ModelResult, error) {
	s.task = task
	return s.result, s.err
}

func (s *stubExecutor) GenerateImage(_ context.Context, _ string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	s.imageRequest = request
	return s.images, s.err
}

func (s *stubExecutor) GenerateVideo(_ context.Context, _ string, request domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	s.videoRequest = request
	return s.videos, s.err
}

func (s *stubExecutor) AgentID() string                                { return "writer-a" }
func (s *stubExecutor) ModelReference() domain.ModelReference          { return domain.ModelReference{} }
func (s *stubExecutor) PromptSections() []domain.CompiledPromptSection { return s.sections }

func textExecutor() *stubExecutor {
	return &stubExecutor{
		sections: []domain.CompiledPromptSection{{Caption: "tone", Instruction: "warm"}},
		result: domain.ModelResult{
			Output: []byte("<p>body</p>"),
			Usage:  domain.Usage{InputTokens: 10, OutputTokens: 20},
		},
	}
}

func TestGenerateTextOnly(t *testing.T) {
	text := textExecutor()
	result, err := MultimodalGenerator{Text: text}.Generate(context.Background(), domain.MultimodalTask{
		AgentTask: domain.AgentTask{Task: "Draft", OutputFormat: "HTML"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Text != "<p>body</p>" || result.Usage.OutputTokens != 20 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Media) != 0 || result.ImageCount != 0 || result.VideoSeconds != 0 {
		t.Fatalf("no media was requested but got %+v", result.Media)
	}
	if text.task.OutputFormat != "HTML" {
		t.Fatalf("product output format must reach the executor, got %q", text.task.OutputFormat)
	}
}

// Media is styled from the agent's own sections and named from the base name.
func TestGenerateWithMediaNamesAndStyles(t *testing.T) {
	text := textExecutor()
	media := &stubExecutor{
		images: []domain.GeneratedMedia{{Data: []byte("i1"), MimeType: "image/png"}, {Data: []byte("i2"), MimeType: "image/jpeg"}},
		videos: []domain.GeneratedMedia{{Data: []byte("v1"), MimeType: "video/mp4"}},
	}
	result, err := MultimodalGenerator{Text: text, Image: media, Video: media}.Generate(context.Background(), domain.MultimodalTask{
		AgentTask:     domain.AgentTask{Task: "Draft"},
		Image:         &domain.ImageRequest{Count: 2, AspectRatio: "16:9"},
		Video:         &domain.VideoRequest{Count: 1, DurationSeconds: 5},
		AssetBaseName: "spring-pergolas",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Video is produced before images, so ordering stays stable for storage.
	want := []string{"spring-pergolas-vid-1.mp4", "spring-pergolas-img-1.png", "spring-pergolas-img-2.jpg"}
	if len(result.Media) != len(want) {
		t.Fatalf("media = %+v", result.Media)
	}
	for i, name := range want {
		if result.Media[i].FileName != name {
			t.Errorf("media[%d] = %s, want %s", i, result.Media[i].FileName, name)
		}
	}
	if result.ImageCount != 2 || result.VideoSeconds != 5 {
		t.Errorf("billable quantities: images=%d videoSeconds=%d", result.ImageCount, result.VideoSeconds)
	}
	if media.imageRequest.StyleHint != "tone: warm" || media.videoRequest.StyleHint != "tone: warm" {
		t.Errorf("media must inherit the agent style: image=%q video=%q", media.imageRequest.StyleHint, media.videoRequest.StyleHint)
	}
}

// An explicit style hint is the caller's choice and must not be overwritten.
func TestGenerateKeepsCallerStyleHint(t *testing.T) {
	media := &stubExecutor{images: []domain.GeneratedMedia{{MimeType: "image/png"}}}
	_, err := MultimodalGenerator{Text: textExecutor(), Image: media}.Generate(context.Background(), domain.MultimodalTask{
		AgentTask:     domain.AgentTask{Task: "Draft"},
		Image:         &domain.ImageRequest{Count: 1, StyleHint: "monochrome"},
		AssetBaseName: "asset",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if media.imageRequest.StyleHint != "monochrome" {
		t.Fatalf("style hint = %q", media.imageRequest.StyleHint)
	}
}

// Requesting a modality the release has no model for is a readiness failure,
// not a silently text-only result.
func TestGenerateRequiresConfiguredMediaModels(t *testing.T) {
	for _, tc := range []struct {
		name string
		task domain.MultimodalTask
	}{
		{"image", domain.MultimodalTask{Image: &domain.ImageRequest{}, AssetBaseName: "a"}},
		{"video", domain.MultimodalTask{Video: &domain.VideoRequest{}, AssetBaseName: "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MultimodalGenerator{Text: textExecutor()}.Generate(context.Background(), tc.task)
			if !errors.Is(err, domain.ErrNotReady) {
				t.Fatalf("want ErrNotReady, got %v", err)
			}
		})
	}
}

func TestGenerateRequiresAssetBaseNameAndTextExecutor(t *testing.T) {
	media := &stubExecutor{}
	_, err := MultimodalGenerator{Text: textExecutor(), Image: media}.Generate(context.Background(), domain.MultimodalTask{
		Image: &domain.ImageRequest{Count: 1},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unnamed media assets must be rejected, got %v", err)
	}
	if _, err = (MultimodalGenerator{}).Generate(context.Background(), domain.MultimodalTask{}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("missing text executor must be ErrNotReady, got %v", err)
	}
}

// A media failure fails the turn; a half-illustrated post is not a success.
func TestGenerateFailsWhenMediaFails(t *testing.T) {
	broken := errors.New("provider down")
	_, err := MultimodalGenerator{Text: textExecutor(), Video: &stubExecutor{err: broken}}.Generate(context.Background(), domain.MultimodalTask{
		AgentTask: domain.AgentTask{Task: "Draft"}, Video: &domain.VideoRequest{Count: 1, DurationSeconds: 5}, AssetBaseName: "a",
	})
	if !errors.Is(err, broken) {
		t.Fatalf("want the provider failure surfaced, got %v", err)
	}
}

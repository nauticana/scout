package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Media extensions by content type; a provider that returns another type gets
// the modality default rather than an extensionless asset.
var mediaExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"video/mp4":  "mp4",
	"video/webm": "webm",
}

// MultimodalGenerator runs one turn across a published agent's text and media
// executors: text first, then any requested media, styled from the agent's own
// compiled prompt sections so illustrations match its configured voice.
type MultimodalGenerator struct {
	Text  contract.AgentExecutor
	Image contract.AgentExecutor
	Video contract.AgentExecutor
}

func (g MultimodalGenerator) Generate(ctx context.Context, task domain.MultimodalTask) (domain.MultimodalResult, error) {
	if g.Text == nil {
		return domain.MultimodalResult{}, fmt.Errorf("%w: multimodal generation requires a text executor", domain.ErrNotReady)
	}
	if task.Image != nil && g.Image == nil {
		return domain.MultimodalResult{}, fmt.Errorf("%w: image generation was requested but no image model is configured", domain.ErrNotReady)
	}
	if task.Video != nil && g.Video == nil {
		return domain.MultimodalResult{}, fmt.Errorf("%w: video generation was requested but no video model is configured", domain.ErrNotReady)
	}
	if (task.Image != nil || task.Video != nil) && strings.TrimSpace(task.AssetBaseName) == "" {
		return domain.MultimodalResult{}, fmt.Errorf("%w: an asset base name is required to generate media", domain.ErrValidation)
	}

	generated, err := g.Text.Generate(ctx, task.AgentTask)
	if err != nil {
		return domain.MultimodalResult{}, err
	}
	result := domain.MultimodalResult{Text: string(generated.Output), Usage: generated.Usage}

	styleHint := StyleHint(g.Text.PromptSections())
	if task.Video != nil {
		request := *task.Video
		if request.StyleHint == "" {
			request.StyleHint = styleHint
		}
		videos, videoErr := g.Video.GenerateVideo(ctx, task.Task, request)
		if videoErr != nil {
			return domain.MultimodalResult{}, fmt.Errorf("generate video: %w", videoErr)
		}
		result.Media = append(result.Media, nameMedia(videos, task.AssetBaseName, "vid", "mp4")...)
		result.VideoSeconds = len(videos) * int(request.DurationSeconds)
	}
	if task.Image != nil {
		request := *task.Image
		if request.StyleHint == "" {
			request.StyleHint = styleHint
		}
		images, imageErr := g.Image.GenerateImage(ctx, task.Task, request)
		if imageErr != nil {
			return domain.MultimodalResult{}, fmt.Errorf("generate image: %w", imageErr)
		}
		result.Media = append(result.Media, nameMedia(images, task.AssetBaseName, "img", "png")...)
		result.ImageCount = len(images)
	}
	return result, nil
}

func nameMedia(media []domain.GeneratedMedia, baseName, kind, defaultExtension string) []domain.NamedMedia {
	named := make([]domain.NamedMedia, 0, len(media))
	for i, item := range media {
		extension, ok := mediaExtensions[item.MimeType]
		if !ok {
			extension = defaultExtension
		}
		named = append(named, domain.NamedMedia{
			GeneratedMedia: item,
			FileName:       fmt.Sprintf("%s-%s-%d.%s", baseName, kind, i+1, extension),
		})
	}
	return named
}

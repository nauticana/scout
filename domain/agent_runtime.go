package domain

// AgentReleaseReference identifies the immutable definition used to build an
// executable agent. Products can persist it alongside their own run records
// without retaining the complete definition.
type AgentReleaseReference struct {
	AgentID string
	Version string
	Digest  string
}

// AgentTask is one request executed against a published agent definition. The
// compiled prompt sections supply the agent's standing configuration; these
// fields supply what is specific to this invocation.
type AgentTask struct {
	Task            string
	Context         string
	InputData       string
	OutputFormat    string
	PastPerformance string
}

// ImageRequest bounds one image generation call.
type ImageRequest struct {
	Prompt      string
	Count       int32
	AspectRatio string
	StyleHint   string
}

// VideoRequest bounds one video generation call.
type VideoRequest struct {
	Prompt          string
	Count           int32
	DurationSeconds int32
	StyleHint       string
}

// GeneratedMedia is one produced image or video with its content type.
type GeneratedMedia struct {
	Data     []byte
	MimeType string
}

// NamedMedia is generated media with a stable asset name for storage.
type NamedMedia struct {
	GeneratedMedia
	FileName string
}

// MultimodalTask is one turn that produces text and, when requested, media
// illustrating it. A nil Image or Video means that modality is not requested;
// the product owns OutputFormat and the asset naming.
type MultimodalTask struct {
	AgentTask
	Image         *ImageRequest
	Video         *VideoRequest
	AssetBaseName string
}

// MultimodalResult carries the text, its usage, and every produced asset.
// ImageCount and VideoSeconds are the billable media quantities.
type MultimodalResult struct {
	Text         string
	Usage        Usage
	Media        []NamedMedia
	ImageCount   int
	VideoSeconds int
}

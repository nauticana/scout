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
	// Output constrains the answer natively; the prompt renderer ignores it.
	Output OutputConstraint
	// Search asks the bound route to answer from the provider's own web search
	// and report its sources on ModelResult.Citations; the prompt renderer
	// ignores it. It requires CapabilityWebSearch of the route.
	Search *SearchGrounding
}

// ImageRequest bounds one image generation call.
type ImageRequest struct {
	TenantContext  TenantContext
	RequestID      string
	ConversationID string
	Prompt         string
	Count          int32
	AspectRatio    string
	StyleHint      string
	// Quality and OutputFormat are the vendor's own values, such as low, medium
	// or high and png, jpeg or webp; empty leaves the vendor default.
	Quality      string
	OutputFormat string
}

// VideoRequest bounds one video generation call.
type VideoRequest struct {
	TenantContext   TenantContext
	RequestID       string
	ConversationID  string
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

// MediaResult is the media one generation call produced and the usage it
// billed: the asset count or seconds, and the tokens of a token-priced model.
type MediaResult struct {
	Media []GeneratedMedia
	Usage ModelUsage
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

// MultimodalResult carries the text, its usage, every produced asset, and the
// usage of each media model, billed on that model's own prices.
type MultimodalResult struct {
	Text string
	// Citations are the sources of a grounded answer, in the provider's order.
	Citations []Citation
	Usage     Usage
	Media     []NamedMedia
	Image     ModelUsage
	Video     ModelUsage
}

package provider

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"

	"github.com/nauticana/scout/domain"
)

func groundedRequest(maxSearches int64) domain.ModelRequest {
	return domain.ModelRequest{
		Prompt: []byte("who ranks for x"), MaxOutputTokens: 64,
		Search: &domain.SearchGrounding{MaxSearches: maxSearches},
	}
}

func requireCitation(t *testing.T, citations []domain.Citation, at int, url, host, title, snippet string) {
	t.Helper()
	if len(citations) <= at {
		t.Fatalf("citations = %+v, want one at %d", citations, at)
	}
	got := citations[at]
	if got.URL != url || got.Domain != host || got.Title != title || got.Snippet != snippet || got.Position != at+1 {
		t.Fatalf("citation[%d] = %+v", at, got)
	}
}

func TestAnthropicGroundsAndReportsCitedSources(t *testing.T) {
	params, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, groundedRequest(3))
	if err != nil {
		t.Fatalf("messageParams: %v", err)
	}
	requireAll(t, encoded(t, params), `"type":"web_search_20250305"`, `"max_uses":3`)

	var message anthropic.Message
	if err := json.Unmarshal([]byte(`{"content":[
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
			{"type":"web_search_result","url":"https://a.example/p","title":"A","encrypted_content":"e","page_age":""},
			{"type":"web_search_result","url":"https://b.example/p","title":"B","encrypted_content":"e","page_age":""}]},
		{"type":"text","text":"a ranks first","citations":[
			{"type":"web_search_result_location","url":"https://a.example/p","title":"A","cited_text":"A is first","encrypted_index":"i"}]}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":5,"output_tokens":7,"server_tool_use":{"web_search_requests":2}}}`), &message); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	result, err := anthropicResult(&message)
	if err != nil {
		t.Fatalf("anthropicResult: %v", err)
	}
	// Only sources cited by the answer are returned, not every search result.
	requireCitation(t, result.Citations, 0, "https://a.example/p", "a.example", "A", "A is first")
	if len(result.Citations) != 1 || result.Usage.SearchQueries != 2 {
		t.Fatalf("citations = %+v usage = %+v", result.Citations, result.Usage)
	}
}

func TestOpenAIGroundsAndRefusesAnUnboundableSearchLimit(t *testing.T) {
	params, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, groundedRequest(0))
	if err != nil {
		t.Fatalf("completionParams: %v", err)
	}
	requireAll(t, encoded(t, params), `"web_search_options":{`, `"search_context_size":"medium"`)

	if _, err = (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, groundedRequest(3)); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("a search limit the vendor cannot enforce must be refused, got %v", err)
	}

	var completion openai.ChatCompletion
	if err := json.Unmarshal([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant",
		"content":"a ranks first","annotations":[
			{"type":"url_citation","url_citation":{"url":"https://a.example/p","title":"A","start_index":0,"end_index":1}}]}}],
		"usage":{"prompt_tokens":5,"completion_tokens":7}}`), &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	result, err := openAIResult(&completion, groundedRequest(0))
	if err != nil {
		t.Fatalf("openAIResult: %v", err)
	}
	requireCitation(t, result.Citations, 0, "https://a.example/p", "a.example", "A", "a")
	if result.Usage.SearchQueries != 1 {
		t.Fatalf("a grounded call is billed once, got %+v", result.Usage)
	}
	if ungrounded, _ := openAIResult(&completion, domain.ModelRequest{}); ungrounded.Usage.SearchQueries != 0 {
		t.Fatalf("an ungrounded call must not be billed a search, got %+v", ungrounded.Usage)
	}
}

func TestGoogleGroundsAndMapsGroundingMetadata(t *testing.T) {
	_, config, err := (&Google{}).contentParams(groundedRequest(0))
	if err != nil {
		t.Fatalf("contentParams: %v", err)
	}
	requireAll(t, encoded(t, config), `"googleSearch":{`)

	if _, _, err = (&Google{}).contentParams(groundedRequest(3)); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("a search limit the vendor cannot enforce must be refused, got %v", err)
	}

	response := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		Content:      &genai.Content{Parts: []*genai.Part{{Text: "a ranks first"}}},
		FinishReason: genai.FinishReasonStop,
		GroundingMetadata: &genai.GroundingMetadata{
			WebSearchQueries: []string{"who ranks for x", "x ranking"},
			GroundingChunks: []*genai.GroundingChunk{
				{Web: &genai.GroundingChunkWeb{URI: "https://a.example/p", Title: "A"}},
				{Maps: &genai.GroundingChunkMaps{}},
				{Web: &genai.GroundingChunkWeb{URI: "https://b.example/p", Title: "B"}},
			},
			GroundingSupports: []*genai.GroundingSupport{
				{GroundingChunkIndices: []int32{0, 2}, Segment: &genai.Segment{Text: "a ranks first"}},
			},
		},
	}}}
	result, err := googleResult(response, groundedRequest(0))
	if err != nil {
		t.Fatalf("googleResult: %v", err)
	}
	// A non-web chunk carries no source, so it takes no citation position.
	requireCitation(t, result.Citations, 0, "https://a.example/p", "a.example", "A", "a ranks first")
	requireCitation(t, result.Citations, 1, "https://b.example/p", "b.example", "B", "a ranks first")
	if result.Usage.SearchQueries != 2 {
		t.Fatalf("usage = %+v, want the two searches Gemini ran", result.Usage)
	}
}

func TestCitationsKeepTheFirstDomainTitleAndSnippetPerURL(t *testing.T) {
	var sources citations
	sources.add("https://a.example/p", "", "", "")
	sources.add(" ", "ignored", "ignored", "ignored")
	sources.add("https://a.example/p", "a.example", "A", "cited")
	sources.add("https://b.example/p", "", "B", "")
	if len(sources.list) != 2 {
		t.Fatalf("citations = %+v", sources.list)
	}
	requireCitation(t, sources.list, 0, "https://a.example/p", "a.example", "A", "cited")
	requireCitation(t, sources.list, 1, "https://b.example/p", "", "B", "")
}

func TestGoogleCitationNamesThePublisherBehindTheRedirect(t *testing.T) {
	redirect := "https://" + googleGroundingRedirect + "/grounding-api-redirect/"
	cases := map[string]struct {
		web  genai.GroundingChunkWeb
		want string
	}{
		"vertex domain":       {genai.GroundingChunkWeb{URI: redirect + "1", Title: "Deck Guide", Domain: "Decks.Example."}, "decks.example"},
		"gemini host title":   {genai.GroundingChunkWeb{URI: redirect + "2", Title: " Builder.Example "}, "builder.example"},
		"direct link":         {genai.GroundingChunkWeb{URI: "https://www.a.example/p", Title: "A"}, "www.a.example"},
		"redirect, no host":   {genai.GroundingChunkWeb{URI: redirect + "3", Title: "Best deck builders"}, ""},
		"redirect, bad title": {genai.GroundingChunkWeb{URI: redirect + "4", Title: "example"}, ""},
	}
	for name, test := range cases {
		if got := googlePublisher(&test.web); got != test.want {
			t.Errorf("%s: publisher = %q, want %q", name, got, test.want)
		}
	}
}

func locatedRequest(location domain.SearchLocation) domain.ModelRequest {
	request := groundedRequest(0)
	request.Search.Location = &location
	return request
}

func TestProvidersSearchFromTheRequestedLocation(t *testing.T) {
	latitude, longitude := 30.2672, -97.7431
	place := domain.SearchLocation{City: "Austin", Region: "Texas", Country: "US", Timezone: "America/Chicago", Latitude: &latitude, Longitude: &longitude}

	openAIParams, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, locatedRequest(place))
	if err != nil {
		t.Fatalf("completionParams: %v", err)
	}
	requireAll(t, encoded(t, openAIParams), `"user_location":{"approximate":{"city":"Austin","country":"US","region":"Texas","timezone":"America/Chicago"},"type":"approximate"}`)

	anthropicParams, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, locatedRequest(place))
	if err != nil {
		t.Fatalf("messageParams: %v", err)
	}
	requireAll(t, encoded(t, anthropicParams), `"user_location":{"city":"Austin","country":"US","region":"Texas","timezone":"America/Chicago","type":"approximate"}`)

	_, googleConfig, err := (&Google{}).contentParams(locatedRequest(place))
	if err != nil {
		t.Fatalf("contentParams: %v", err)
	}
	requireAll(t, encoded(t, googleConfig), `"retrievalConfig":{"latLng":{"latitude":30.2672,"longitude":-97.7431}}`)
}

func TestProvidersRefuseALocationTheirVendorCannotHonour(t *testing.T) {
	latitude, longitude := 30.2672, -97.7431
	named := locatedRequest(domain.SearchLocation{City: "Austin", Country: "US"})
	if _, _, err := (&Google{}).contentParams(named); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("Google without coordinates: error = %v", err)
	}
	coordinates := locatedRequest(domain.SearchLocation{Latitude: &latitude, Longitude: &longitude})
	if _, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, coordinates); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("OpenAI with coordinates only: error = %v", err)
	}
	if _, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, coordinates); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("Anthropic with coordinates only: error = %v", err)
	}
}

func TestProvidersRejectAMalformedLocation(t *testing.T) {
	request := locatedRequest(domain.SearchLocation{City: "Austin", Country: "usa"})
	if _, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Anthropic error = %v", err)
	}
	if _, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("OpenAI error = %v", err)
	}
	if _, _, err := (&Google{}).contentParams(request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Google error = %v", err)
	}
}

func TestProvidersSendInstructionsInTheSystemSlot(t *testing.T) {
	request := domain.ModelRequest{Instructions: []byte("name businesses"), Prompt: []byte("best deck builder"), MaxOutputTokens: 64}

	openAIParams, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil {
		t.Fatalf("completionParams: %v", err)
	}
	requireAll(t, encoded(t, openAIParams), `"messages":[{"content":"name businesses","role":"system"},{"content":"best deck builder","role":"user"}]`)

	anthropicParams, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil {
		t.Fatalf("messageParams: %v", err)
	}
	requireAll(t, encoded(t, anthropicParams), `"system":[{"text":"name businesses","type":"text"}]`, `"messages":[{"content":[{"text":"best deck builder","type":"text"}],"role":"user"}]`)

	contents, googleConfig, err := (&Google{}).contentParams(request)
	if err != nil {
		t.Fatalf("contentParams: %v", err)
	}
	requireAll(t, encoded(t, googleConfig), `"systemInstruction":{"parts":[{"text":"name businesses"}]`)
	if len(contents) != 1 || contents[0].Parts[0].Text != "best deck builder" {
		t.Fatalf("contents = %+v, want the prompt alone", contents)
	}
}

func TestProvidersRejectNegativeSearchLimits(t *testing.T) {
	request := groundedRequest(-1)
	if _, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Anthropic error = %v", err)
	}
	if _, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("OpenAI error = %v", err)
	}
	if _, _, err := (&Google{}).contentParams(request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Google error = %v", err)
	}
}

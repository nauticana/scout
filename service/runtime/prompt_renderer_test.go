package runtime

import (
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func sections() []domain.CompiledPromptSection {
	return []domain.CompiledPromptSection{
		{Sequence: 1, PromptSectionID: 1, Caption: "task", Description: "Task", Instruction: "write a post"},
		{Sequence: 2, PromptSectionID: 2, Caption: "tone", Description: "Tone of voice", Instruction: "warm"},
	}
}

// The rendered layout is published behavior; this pins it byte for byte.
func TestRenderLayoutIsFrozen(t *testing.T) {
	got := PromptRenderer{}.Render("writer-a", sections(), domain.AgentTask{
		Task: "Draft", Context: "spring", InputData: "pergolas",
		PastPerformance: "older post", OutputFormat: "markdown",
	})
	want := "Task: Draft\n" +
		"Context: spring\n" +
		"Input Data: pergolas\n" +
		"\n--- Agent Configuration ---\n" +
		"Agent Name: writer-a\n" +
		"task - Task: write a post\n" +
		"tone - Tone of voice: warm\n" +
		"\n--- Past High-Performing Content ---\n: older post\n" +
		"\nOutput Format: markdown\n"
	if got != want {
		t.Fatalf("render drifted from the published layout:\n got %q\nwant %q", got, want)
	}
}

// Absent task fields emit no line at all, not an empty label.
func TestRenderOmitsEmptyTaskFields(t *testing.T) {
	got := PromptRenderer{}.Render("", sections()[:1], domain.AgentTask{Task: "Draft"})
	for _, absent := range []string{"Context:", "Input Data:", "Agent Name:", "Past High-Performing", "Output Format:"} {
		if strings.Contains(got, absent) {
			t.Errorf("render emitted %q for an unset field:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "task - Task: write a post") {
		t.Errorf("configured sections must always render:\n%s", got)
	}
}

func TestStyleHintSkipsEmptyInstructions(t *testing.T) {
	got := StyleHint([]domain.CompiledPromptSection{
		{Caption: "tone", Instruction: "warm"},
		{Caption: "brand", Instruction: ""},
	})
	if got != "tone: warm" {
		t.Fatalf("style hint = %q", got)
	}
}

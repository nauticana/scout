package runtime

import (
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PromptRenderer renders compiled sections and a task into a provider payload.
//
// The layout below is the published contract of every agent released against
// it. Reordering or relabelling a line changes model output for agents that
// were validated under the old shape, so treat it as frozen and introduce a
// second renderer rather than editing this one.
type PromptRenderer struct{}

var _ contract.PromptRenderer = (*PromptRenderer)(nil)

func (PromptRenderer) Render(agentID string, sections []domain.CompiledPromptSection, task domain.AgentTask) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\n", task.Task)
	if task.Context != "" {
		fmt.Fprintf(&sb, "Context: %s\n", task.Context)
	}
	if task.InputData != "" {
		fmt.Fprintf(&sb, "Input Data: %s\n", task.InputData)
	}

	sb.WriteString("\n--- Agent Configuration ---\n")
	if agentID != "" {
		fmt.Fprintf(&sb, "Agent Name: %s\n", agentID)
	}
	for _, section := range sections {
		fmt.Fprintf(&sb, "%s - %s: %s\n", section.Caption, section.Description, section.Instruction)
	}

	if task.PastPerformance != "" {
		fmt.Fprintf(&sb, "\n--- Past High-Performing Content ---\n: %s\n", task.PastPerformance)
	}
	if task.OutputFormat != "" {
		fmt.Fprintf(&sb, "\nOutput Format: %s\n", task.OutputFormat)
	}
	return sb.String()
}

// StyleHint condenses compiled sections into a one-line-per-section hint for
// media prompts, which take no structured configuration.
func StyleHint(sections []domain.CompiledPromptSection) string {
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		if section.Instruction != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", section.Caption, section.Instruction))
		}
	}
	return strings.Join(parts, "\n")
}

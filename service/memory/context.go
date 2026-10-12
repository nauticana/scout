package memory

import (
	"fmt"
	"strings"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/guardrail"
)

// Context renders a tenant's memory for a task, each section fenced as untrusted data; no
// sections render nothing, and memory over maxBytes fails rather than being cut.
func Context(sections []domain.MemorySection, maxBytes int) (string, error) {
	var out strings.Builder
	for _, section := range sections {
		label := section.Caption
		if strings.TrimSpace(label) == "" {
			label = section.SectionID
		}
		out.WriteString(guardrail.MarkUntrusted(label, section.Body))
	}
	if out.Len() == 0 {
		return "", nil
	}
	rendered := guardrail.UntrustedContentNotice + "What this tenant's agents have learned:\n" + out.String()
	if maxBytes > 0 && len(rendered) > maxBytes {
		return "", fmt.Errorf("%w: tenant memory exceeds %d bytes", domain.ErrExecutionLimit, maxBytes)
	}
	return strings.TrimSuffix(rendered, "\n"), nil
}

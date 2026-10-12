package domain

import (
	"fmt"
	"time"
)

// MemorySource is who vouches for a remembered fact.
type MemorySource string

const (
	// MemoryStated is a fact a person stated.
	MemoryStated MemorySource = "user"
	// MemoryVerified is a fact a governed tool returned.
	MemoryVerified MemorySource = "tool"
)

// ErrUnprovenFact refuses a fact nobody vouches for.
var ErrUnprovenFact = fmt.Errorf("%w: a remembered fact needs a person or a tool behind it", ErrValidation)

// MemorySection is one named part of a tenant's memory.
type MemorySection struct {
	SectionID string    `json:"section_id"`
	Caption   string    `json:"caption"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryFact is one line appended to a section; Actor names the person or tool behind it.
type MemoryFact struct {
	TenantID  int64
	SectionID string
	Fact      string
	Source    MemorySource
	Actor     string
}

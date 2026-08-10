package domain

import "time"

// AuditEvent contains redacted tenant-scoped audit data.
type AuditEvent struct {
	TenantID   int64
	Category   string
	Payload    []byte
	OccurredAt time.Time
}

package confirmation

import "time"

// NewFakeTableStore returns a TableStore over the in-memory query interpreter and its clock.
func NewFakeTableStore() (*TableStore, func(time.Duration)) {
	db := newConfirmationDB()
	return &TableStore{DB: db}, db.advance
}

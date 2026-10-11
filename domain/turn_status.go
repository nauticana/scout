package domain

// TurnStatus is the lifecycle code of one durable turn: a turn_status code.
type TurnStatus string

const (
	TurnQueued    TurnStatus = "Q"
	TurnRunning   TurnStatus = "R"
	TurnStreaming TurnStatus = "S"
	TurnSuspended TurnStatus = "U"
	TurnCompleted TurnStatus = "C"
	TurnFailed    TurnStatus = "F"
	TurnCancelled TurnStatus = "X"
)

// Valid reports whether the status is a seeded turn_status code.
func (status TurnStatus) Valid() bool {
	switch status {
	case TurnQueued, TurnRunning, TurnStreaming, TurnSuspended, TurnCompleted, TurnFailed, TurnCancelled:
		return true
	}
	return false
}

// Terminal reports whether the status is one a turn never leaves.
func (status TurnStatus) Terminal() bool {
	return status == TurnCompleted || status == TurnFailed || status == TurnCancelled
}

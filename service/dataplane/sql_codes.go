package dataplane

import "github.com/nauticana/scout/domain"

// SQL literals of the closed codes the data plane's statements name.
const (
	sqlTurnQueued    = "'" + string(domain.TurnQueued) + "'"
	sqlTurnRunning   = "'" + string(domain.TurnRunning) + "'"
	sqlTurnStreaming = "'" + string(domain.TurnStreaming) + "'"
	sqlTurnSuspended = "'" + string(domain.TurnSuspended) + "'"
	sqlTurnCompleted = "'" + string(domain.TurnCompleted) + "'"
	sqlTurnFailed    = "'" + string(domain.TurnFailed) + "'"

	sqlQueueQueued = "'" + string(domain.QueueQueued) + "'"
	sqlQueueLeased = "'" + string(domain.QueueLeased) + "'"
	sqlQueueAcked  = "'" + string(domain.QueueAcked) + "'"
	sqlQueueDead   = "'" + string(domain.QueueDead) + "'"

	sqlStepClaimed   = "'" + string(domain.StepClaimed) + "'"
	sqlStepCommitted = "'" + string(domain.StepCommitted) + "'"
	sqlStepAbandoned = "'" + string(domain.StepAbandoned) + "'"

	sqlReservationHeld     = "'" + string(domain.ReservationHeld) + "'"
	sqlReservationSettled  = "'" + string(domain.ReservationSettled) + "'"
	sqlReservationReleased = "'" + string(domain.ReservationReleased) + "'"
	sqlReservationExpired  = "'" + string(domain.ReservationExpired) + "'"
)

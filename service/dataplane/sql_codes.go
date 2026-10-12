package dataplane

import "github.com/nauticana/scout/domain"

// SQL literals of the closed codes the data plane's statements name.
const (
	sqlTurnQueued    = "'" + string(domain.TurnQueued) + "'"
	sqlTurnRunning   = "'" + string(domain.TurnRunning) + "'"
	sqlTurnStreaming = "'" + string(domain.TurnStreaming) + "'"
	sqlTurnSuspended = "'" + string(domain.TurnSuspended) + "'"
	sqlTurnHeld      = "'" + string(domain.TurnHeld) + "'"
	sqlTurnCompleted = "'" + string(domain.TurnCompleted) + "'"
	sqlTurnFailed    = "'" + string(domain.TurnFailed) + "'"

	// sqlTurnOwned lists the statuses a worker owns a turn in; sqlTurnLive every non-terminal one.
	sqlTurnOwned = sqlTurnRunning + ", " + sqlTurnStreaming
	sqlTurnLive  = sqlTurnQueued + ", " + sqlTurnRunning + ", " + sqlTurnStreaming + ", " + sqlTurnSuspended + ", " + sqlTurnHeld

	sqlHoldCheckpoint = "'" + string(domain.HoldCheckpoint) + "'"

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

	sqlAgentActive   = "'" + string(domain.AgentStateActive) + "'"
	sqlAlertOpen     = "'" + string(domain.AlertOpen) + "'"
	sqlAlertResolved = "'" + string(domain.AlertResolved) + "'"
	sqlAlertFailure  = "'" + string(domain.AlertFailureRate) + "'"
	sqlAlertLatency  = "'" + string(domain.AlertLatency) + "'"
	sqlAlertSafety   = "'" + string(domain.AlertSafety) + "'"
	sqlAlertSilence  = "'" + string(domain.AlertSilence) + "'"
)

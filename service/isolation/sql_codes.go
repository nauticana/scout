package isolation

import "github.com/nauticana/scout/domain"

// SQL literals of the reservation_status and turn_status codes the budget statements name.
const (
	sqlReservationHeld     = "'" + string(domain.ReservationHeld) + "'"
	sqlReservationSettled  = "'" + string(domain.ReservationSettled) + "'"
	sqlReservationReleased = "'" + string(domain.ReservationReleased) + "'"
	sqlReservationExpired  = "'" + string(domain.ReservationExpired) + "'"

	sqlTurnOwned = "'" + string(domain.TurnRunning) + "', '" + string(domain.TurnStreaming) + "'"
)

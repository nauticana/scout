package isolation

import "github.com/nauticana/scout/domain"

// SQL literals of the reservation_status and principal kind codes the budget statements name.
const (
	sqlReservationHeld     = "'" + string(domain.ReservationHeld) + "'"
	sqlReservationSettled  = "'" + string(domain.ReservationSettled) + "'"
	sqlReservationReleased = "'" + string(domain.ReservationReleased) + "'"
	sqlReservationExpired  = "'" + string(domain.ReservationExpired) + "'"

	sqlPrincipalAgent = "'" + string(domain.PrincipalAgent) + "'"
	sqlPrincipalHuman = "'" + string(domain.PrincipalHuman) + "'"
)

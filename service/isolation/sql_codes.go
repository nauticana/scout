package isolation

import "github.com/nauticana/scout/domain"

// SQL literals of the reservation_status codes the budget statements name.
const (
	sqlReservationHeld     = "'" + string(domain.ReservationHeld) + "'"
	sqlReservationSettled  = "'" + string(domain.ReservationSettled) + "'"
	sqlReservationReleased = "'" + string(domain.ReservationReleased) + "'"
	sqlReservationExpired  = "'" + string(domain.ReservationExpired) + "'"
)

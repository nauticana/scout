package domain

// ReservationStatus is the lifecycle code of one budget reservation: a reservation_status code.
type ReservationStatus string

const (
	ReservationHeld     ReservationStatus = "H"
	ReservationSettled  ReservationStatus = "S"
	ReservationReleased ReservationStatus = "R"
	ReservationExpired  ReservationStatus = "E"
)

// Valid reports whether the status is a seeded reservation_status code.
func (status ReservationStatus) Valid() bool {
	return status == ReservationHeld || status == ReservationSettled || status == ReservationReleased || status == ReservationExpired
}

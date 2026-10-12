package domain

// TurnSettlement is how a turn's end settles its budget, written in the transaction that records
// the end so a reservation is never settled while the end can still be refused. The reservation
// is committed at Usage, or released when Refund is set on a failed turn that spent nothing; the
// usage events are written unless the turn is refunded. A zero Reservation settles no hold.
type TurnSettlement struct {
	Reservation BudgetReservation
	Usage       Usage
	Refund      bool
	SubjectRef  string
	Attribution UsageAttribution
}

package domain

// TurnQueueStatus is the delivery state of one turn_queue row: a turn_queue_status code.
type TurnQueueStatus string

const (
	QueueQueued TurnQueueStatus = "Q"
	QueueLeased TurnQueueStatus = "L"
	QueueAcked  TurnQueueStatus = "A"
	QueueDead   TurnQueueStatus = "D"
)

// Valid reports whether the status is a seeded turn_queue_status code.
func (status TurnQueueStatus) Valid() bool {
	return status == QueueQueued || status == QueueLeased || status == QueueAcked || status == QueueDead
}

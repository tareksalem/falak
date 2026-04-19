package node

type Event[P interface{}] struct {
	ID            string
	Type          string
	Timestamp     int64
	Payload       P
	Metadata      map[string]interface{}
	CorrelationID string
}

type Manager[P interface{}] interface {
	Init() error
	Stop() error
	// GetStatus() S
	ProcessEvent(event Event[P]) error
	Subscribe(cb func(Event[P])) Manager[P]
}

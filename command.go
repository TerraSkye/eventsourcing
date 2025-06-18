package eventsourcing

type Command interface {
	AggregateID() string
}

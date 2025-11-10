package eventsourcing

type StreamRevisionConflictError struct {
}

func (s StreamRevisionConflictError) Error() string {
	return "stream revision conflict"
}

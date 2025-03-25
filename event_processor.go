package eventsourcing

// EventProcessor determines which EventHandler should handle event received from event bus.
type EventProcessor struct {
	handlers []EventHandler
}

// AddHandlers adds a new EventHandler to the EventProcessor and adds it to the router.
func (p *EventProcessor) AddHandlers(handlers ...EventHandler) error {
	p.handlers = append(p.handlers, handlers...)

	return nil
}

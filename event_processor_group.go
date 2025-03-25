package eventsourcing

// EventGroupProcessor determines which EventHandler should handle event received from event bus.
// Compared to EventProcessor, EventGroupProcessor allows to have multiple handlers that share the same subscriber instance.
type EventGroupProcessor struct {
	groupName          string
	groupEventHandlers []GroupEventHandler
}

// NewEventGroupProcessorWithConfig creates a new EventGroupProcessor.
func NewEventGroupProcessor(groupName string, handlers ...GroupEventHandler) *EventGroupProcessor {
	return &EventGroupProcessor{
		groupName:          groupName,
		groupEventHandlers: handlers,
	}
}

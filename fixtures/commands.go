package fixtures

// TestCommand is a configurable test command implementing the Command interface.
type TestCommand struct {
	ID   string
	Data string
}

func (c TestCommand) AggregateID() string { return c.ID }

// TestCommandBuilder provides a fluent API for constructing test commands.
type TestCommandBuilder struct {
	id   string
	data string
}

// NewTestCommand creates a new TestCommandBuilder with sensible defaults.
func NewTestCommand() *TestCommandBuilder {
	return &TestCommandBuilder{
		id:   "aggregate-1",
		data: "",
	}
}

// WithID sets the aggregate ID.
func (b *TestCommandBuilder) WithID(id string) *TestCommandBuilder {
	b.id = id
	return b
}

// WithData sets custom data on the command.
func (b *TestCommandBuilder) WithData(data string) *TestCommandBuilder {
	b.data = data
	return b
}

// Build constructs the TestCommand.
func (b *TestCommandBuilder) Build() TestCommand {
	return TestCommand{
		ID:   b.id,
		Data: b.data,
	}
}

// Common pre-built commands for quick testing.
var (
	CreateOrderCmd = TestCommand{ID: "order-1", Data: "create"}
	UpdateOrderCmd = TestCommand{ID: "order-1", Data: "update"}
	DeleteOrderCmd = TestCommand{ID: "order-1", Data: "delete"}

	CreateUserCmd = TestCommand{ID: "user-1", Data: "create"}
	UpdateUserCmd = TestCommand{ID: "user-1", Data: "update"}
)

// DomainCommand is a more realistic test command with additional fields.
type DomainCommand struct {
	AggID   string
	Action  string
	Payload map[string]any
}

func (c DomainCommand) AggregateID() string { return c.AggID }

// DomainCommandBuilder provides a fluent API for constructing domain commands.
type DomainCommandBuilder struct {
	aggID   string
	action  string
	payload map[string]any
}

// NewDomainCommand creates a new DomainCommandBuilder.
func NewDomainCommand() *DomainCommandBuilder {
	return &DomainCommandBuilder{
		aggID:   "aggregate-1",
		action:  "default",
		payload: make(map[string]any),
	}
}

// WithAggregateID sets the aggregate ID.
func (b *DomainCommandBuilder) WithAggregateID(id string) *DomainCommandBuilder {
	b.aggID = id
	return b
}

// WithAction sets the action/command name.
func (b *DomainCommandBuilder) WithAction(action string) *DomainCommandBuilder {
	b.action = action
	return b
}

// WithPayload sets the entire payload.
func (b *DomainCommandBuilder) WithPayload(payload map[string]any) *DomainCommandBuilder {
	b.payload = payload
	return b
}

// WithField adds a single field to the payload.
func (b *DomainCommandBuilder) WithField(key string, value any) *DomainCommandBuilder {
	b.payload[key] = value
	return b
}

// Build constructs the DomainCommand.
func (b *DomainCommandBuilder) Build() DomainCommand {
	return DomainCommand{
		AggID:   b.aggID,
		Action:  b.action,
		Payload: b.payload,
	}
}

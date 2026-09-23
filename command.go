package eventsourcing

// Command represents a user or system's intent to perform an action on a
// specific domain entity.
//
// A Command models intent, not implementation: name it after what the user
// or system wants to achieve, in terms meaningful to the domain, rather than
// generic or mechanical terms like "Updated" or "Change". For example:
//
//	Command Name      Intent
//	---------------- ----------------------------------------
//	ReserveSeat       Reserve a specific seat
//	CancelOrder       Cancel an existing order
//	ApproveUser       Approve a user registration
//	MarkInvoicePaid   Mark an invoice as paid
//	ShipOrder         Ship a customer order
//	CreateAccount     Create a new user account
//
// Every Command must identify the entity it targets through
// [Command.AggregateID], so a handler can locate that entity's state, and
// name itself through [Command.CommandType], which a [CommandBus] uses to
// route it to the handler registered for it. A Command should be immutable
// after creation, representing a fixed intention at a point in time, and
// self-contained, carrying everything needed to handle it rather than
// relying on hidden state. For example:
//
//	type ReserveSeat struct {
//		ScreeningID string
//		SeatNumber  string
//		UserID      string
//	}
//
//	func (c ReserveSeat) AggregateID() string {
//		return c.ScreeningID
//	}
//
//	func (c ReserveSeat) CommandType() string {
//		return "ReserveSeat"
//	}
type Command interface {
	// AggregateID returns the ID of the entity this command targets, used to
	// locate that entity's state when handling the command.
	AggregateID() string
	// CommandType returns the name under which this command's concrete type is
	// registered; see [Register]. It must be stable across instances, since it
	// is derived from the zero value at registration time.
	CommandType() string
}

package eventsourcing

// Command represents a user or system intention to perform an action on a specific domain entity.
//
// A Command models **intent**, not implementation. It should describe **what the user or system wants to achieve**,
// in terms that are meaningful to the business or domain experts.
//
// Key guidelines for designing Commands:
//
// 1. **Name by intent, not technical detail**:
//
//   - The name should clearly describe the intention or purpose of the action.
//
//   - Avoid generic or mechanical terms like "Updated" or "Change".
//
//     Example command names and their intent:
//
// Example command names and their intent:
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
// 2. **Aggregate target**:
//   - Every Command must identify the entity it targets through `AggregateID()` (or a similar method in your domain model).
//   - This allows the handler to locate the relevant state.
//
// 3. **Immutability**:
//   - Commands should be immutable after creation. They represent a fixed intention at a point in time.
//
// 4. **Self-contained**:
//   - Include all information necessary for the Command to be handled correctly.
//   - Do not rely on hidden state or external assumptions.
//
// Example:
//
//	type ReserveSeat struct {
//	    ScreeningID string
//	    SeatNumber  string
//	    UserID      string
//	}
//
//	func (c ReserveSeat) AggregateID() string {
//	    return c.ScreeningID
//	}
type Command interface {
	// AggregateID returns the ID of the entity this command targets.
	// This ID is used to locate the entity in the system for processing the command.
	AggregateID() string
}

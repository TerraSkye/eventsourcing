// Package postgres keeps events and projections in PostgreSQL: an event
// store and a projection store that share one database.
//
// Apply [Schema] before use. [EventStore] appends and reads the events
// table, and [ProjectionStore] keeps projection checkpoints next to the
// read models. Each projection Commit runs in a single Postgres
// transaction, so the read model's writes and the checkpoint move land
// together. Handlers reach the batch's transaction with [Tx]:
//
//	func handle(ctx context.Context, event eventsourcing.Event) error {
//		switch e := event.(type) {
//		case *events.TaskCreated:
//			_, err := postgres.Tx(ctx).Exec(ctx,
//				`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//				e.TaskID, e.Title)
//			return err
//		}
//		return nil
//	}
//
// The read model's tables must live in the same database as the tables
// [Schema] creates, since they are written in one transaction.
package postgres

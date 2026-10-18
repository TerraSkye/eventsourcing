package postgres

import _ "embed"

// Schema creates every table the package uses: the events table of
// [EventStore], the subscriptions table and insert trigger of [EventBus],
// and the segment and dead-letter tables of [ProjectionStore]. It is
// idempotent; run it with your migrations, or once at startup. The same
// SQL ships as schema.sql in this package's directory.
//
//go:embed schema.sql
var Schema string

package postgres

import _ "embed"

// Schema creates every table the package uses: the events table of
// [EventStore], and the segment and dead-letter tables of
// [ProjectionStore]. It is
// idempotent; run it with your migrations, or once at startup. The same
// SQL ships as schema.sql in this package's directory.
//
// Schema has the tables as this release uses them, and only creates what
// is missing: it adds no column to a table an earlier release created.
// The changelog lists the ALTER TABLE statements to run when upgrading.
//
//go:embed schema.sql
var Schema string

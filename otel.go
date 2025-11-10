package eventsourcing

import "go.opentelemetry.io/otel"

var tracer = otel.Tracer("github.com/terraskye/eventsourcing")

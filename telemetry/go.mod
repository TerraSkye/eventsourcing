module github.com/terraskye/eventsourcing/telemetry

go 1.24.0

require (
	github.com/terraskye/eventsourcing v0.1.8
	go.opentelemetry.io/otel v1.41.0
	go.opentelemetry.io/otel/metric v1.41.0
	go.opentelemetry.io/otel/trace v1.41.0
)

require (
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
)

// Develop against the module in the parent directory; see go.work. Bump
// the require above to the tagged version when releasing.
replace github.com/terraskye/eventsourcing => ../

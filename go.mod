module github.com/terraskye/eventsourcing

go 1.24.1

replace github.com/io-da/query => github.com/terraskye/query v0.0.0-20250310130952-cd3f17f32d38

require (
	github.com/google/uuid v1.6.0
	github.com/io-da/query v1.3.5
	go.opentelemetry.io/otel v1.35.0
	go.opentelemetry.io/otel/trace v1.35.0
)

require (
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.35.0 // indirect
)

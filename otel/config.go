package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// config holds the telemetry options for a single wrapped endpoint, built up
// from the [Option] values passed to a constructor such as
// [WithCommandTelemetry] or [WithEventStoreTelemetry].
type config struct {
	// Operation is the default span name for the endpoint.
	Operation string

	// GetOperation, if non-nil, derives the span name from the context and
	// the existing operation name. If it returns an empty string, the
	// existing operation name is used instead.
	GetOperation func(ctx context.Context, operation string) string

	// Attributes are added to every span created for the endpoint.
	Attributes []attribute.KeyValue

	// GetAttributes, if non-nil, is called for each span to derive
	// additional attributes from the context.
	GetAttributes func(ctx context.Context) []attribute.KeyValue
}

// Option configures the telemetry behavior of a wrapper such as
// [WithCommandTelemetry], [WithQueryTelemetry], [WithEventBusTelemetry], or
// [WithEventStoreTelemetry].
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithOperation sets a fixed span name for the wrapped endpoint, overriding
// the package's default naming.
func WithOperation(operation string) Option {
	return optionFunc(func(o *config) {
		o.Operation = operation
	})
}

// WithOperationGetter sets a function that derives the span name at call
// time from the context and the endpoint's current operation name.
func WithOperationGetter(fn func(ctx context.Context, name string) string) Option {
	return optionFunc(func(o *config) {
		o.GetOperation = fn
	})
}

// WithAttributes sets attributes to add to every span created by the
// wrapped endpoint.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return optionFunc(func(o *config) {
		o.Attributes = attrs
	})
}

// WithAttributeGetter sets a function that derives additional span
// attributes from the context at call time.
func WithAttributeGetter(fn func(ctx context.Context) []attribute.KeyValue) Option {
	return optionFunc(func(o *config) {
		o.GetAttributes = fn
	})
}

package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// config holds the options for tracing an endpoint.
type config struct {
	// Operation identifies the current operation and serves as a span name.
	Operation string

	// GetOperation is an optional function that can set the span name based on the existing operation
	// for the endpoint and information in the context.
	//
	// If the function is nil, or the returned operation is empty, the existing operation for the endpoint is used.
	GetOperation func(ctx context.Context, operation string) string

	// Attributes holds the default attributes for each span created by this middleware.
	Attributes []attribute.KeyValue

	// GetAttributes is an optional function that can extract trace attributes
	// from the context and add them to the span.
	GetAttributes func(ctx context.Context) []attribute.KeyValue
}

// Option configures an EndpointMiddleware.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithOperation sets an operation name for an endpoint.
// Use this when you register a middleware for each endpoint.
func WithOperation(operation string) Option {
	return optionFunc(func(o *config) {
		o.Operation = operation
	})
}

// WithOperationGetter sets an operation name getter function in config.
func WithOperationGetter(fn func(ctx context.Context, name string) string) Option {
	return optionFunc(func(o *config) {
		o.GetOperation = fn
	})
}

// WithAttributes sets the default attributes for the spans created by the Endpoint tracer.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return optionFunc(func(o *config) {
		o.Attributes = attrs
	})
}

// WithAttributeGetter extracts additional attributes from the context.
func WithAttributeGetter(fn func(ctx context.Context) []attribute.KeyValue) Option {
	return optionFunc(func(o *config) {
		o.GetAttributes = fn
	})
}

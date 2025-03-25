package eventsourcing

import (
	"context"
	"github.com/io-da/query"
)

type GenericQueryGateway[T query.Query, R any] struct {
	bus *query.Bus
}

func NewQueryGateway[T query.Query, R any](bus *query.Bus) GenericQueryGateway[T, R] {
	return GenericQueryGateway[T, R]{bus: bus}
}

func (h *GenericQueryGateway[T, R]) Query(ctx context.Context, qry T) (Res[R], error) {
	result, err := h.bus.Query(ctx, qry)

	var envelope Res[R]

	if err != nil {
		return envelope, err
	}

	if resultSize := len(result.All()); resultSize > 0 {

		envelope = Res[R]{make([]*R, resultSize)}
		for i, entity := range result.All() {
			envelope.results[i] = entity.(*R)
		}
	}

	return envelope, nil
}

func (h *GenericQueryGateway[T, R]) IteratorQuery(ctx context.Context, qry T) (<-chan *R, error) {

	result, err := h.bus.IteratorQuery(ctx, qry)

	if err != nil {
		return nil, err
	}

	output := make(chan *R)

	for res := range result.Iterate() {
		output <- res.(*R)
	}

	return output, nil
}

type Res[T any] struct {
	results []*T
}

func (r Res[T]) First() *T {
	if len(r.results) > 0 {
		return r.results[0]
	} else {
		return nil
	}
}

func (r Res[T]) All() []*T {
	return r.results
}

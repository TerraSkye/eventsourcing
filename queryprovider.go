package eventsourcing

import (
	"context"

	"github.com/io-da/query"
)

type QueryHandler[T query.Query, R ReadModel | Iterator[query.Query, ReadModel]] interface {
	HandleQuery(ctx context.Context, qry T) (R, error)
}

//
//type QueryIteratorProvider interface {\
//	query.IteratorHandler
//	RegisterHandler(handler QueryHandler[query.Query, ReadModel])
//}
//
//type QueryProvider interface {
//	query.Handler
//	RegisterHandler(handler QueryHandler[query.Query, ReadModel])
//}
//
//type handler struct {
//	handlers map[string]QueryHandler[query.Query, ReadModel]
//}
//
//func NewQueryHandler() QueryProvider {
//	return &handler{
//		handlers: make(map[string]QueryHandler[query.Query, ReadModel]),
//	}
//}
//
//func (t *handler) RegisterHandler(handler QueryHandler[query.Query, ReadModel]) {
//	var cmd query.Query
//	queryType := TypeName(cmd)
//	// Store a type-erased function that preserves the correct signature
//	if _, ok := t.handlers[queryType]; ok {
//		panic("duplicate query handler" + queryType)
//	}
//	t.handlers[queryType] = handler
//}
//
//func (t *handler) Handle(ctx context.Context, qry query.Query, res *query.Result) error {
//	TypeName(qry)
//
//	provider, exists := t.handlers[TypeName(qry)]
//
//	if !exists {
//		return fmt.Errorf("unknown query type: %s", TypeName(qry))
//	}
//
//	result, err := provider.HandleQuery(ctx, qry)
//
//	if err != nil {
//		return err
//	}
//
//	res.Add(result)
//	res.Done()
//
//	return nil
//}
//
//type iteratorHandler struct {
//	handlers map[string]QueryHandler[query.Query, ReadModel]
//}
//
//func NewQueryIteratorHandler() QueryIteratorProvider {
//	return &iteratorHandler{
//		handlers: make(map[string]QueryHandler[query.Query, ReadModel]),
//	}
//}
//
//func (t *iteratorHandler) RegisterHandler(handler QueryHandler[query.Query, ReadModel]) {
//	var cmd query.Query
//	queryType := TypeName(cmd)
//	// Store a type-erased function that preserves the correct signature
//	if _, ok := t.handlers[queryType]; ok {
//		panic("duplicate query handler" + queryType)
//	}
//	t.handlers[queryType] = handler
//}
//
//func (t *iteratorHandler) Handle(ctx context.Context, qry query.Query, res *query.IteratorResult) error {
//	TypeName(qry)
//
//	provider, exists := t.handlers[TypeName(qry)]
//
//	if !exists {
//		return fmt.Errorf("unknown query type: %s", TypeName(qry))
//	}
//
//	result, err := provider.HandleQuery(ctx, qry)
//
//	if err != nil {
//		return err
//	}
//
//	res.Yield(result)
//
//	res.Done()
//
//	return nil
//}

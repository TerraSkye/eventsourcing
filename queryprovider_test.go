package eventsourcing_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

type Query struct {
}

func (q Query) ID() []byte {
	return uuid.New().NodeID()
}

type Rm struct {
}

type singleRmHandler struct {
}

func (s *singleRmHandler) HandleQuery(ctx context.Context, qry Query) (Rm, error) {

	return Rm{}, nil
}

type multiRmHandler struct {
}

func (s *multiRmHandler) HandleQuery(ctx context.Context, qry Query) (eventsourcing.Iterator[Rm, Query], error) {

	v := eventsourcing.Iterator[Rm, Query]{}
	return v, nil
}

func TestInterface(t *testing.T) {

	a1 := singleRmHandler{}
	a2 := singleRmHandler{}

	a1.HandleQuery(context.Background(), Query{})
	a2.HandleQuery(context.Background(), Query{})
}

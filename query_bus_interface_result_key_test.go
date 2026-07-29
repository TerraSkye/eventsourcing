package eventsourcing

import (
	"context"
	"errors"
	"testing"
)

type ifaceQuery struct{ ID_ string }

func (q ifaceQuery) ID() []byte { return []byte(q.ID_) }

// Two distinct, interface-typed result types for the SAME query type.
type taskView interface{ TaskTitle() string }
type userView interface{ UserName() string }

type taskViewImpl struct{ title string }

func (t taskViewImpl) TaskTitle() string { return t.title }

type userViewImpl struct{ name string }

func (u userViewImpl) UserName() string { return u.name }

// TestQueryBusKey_InterfaceResultCollapsesToNil is a regression test for
// GitHub issue #53: the registry key used to be built with
// fmt.Sprintf("%T|%T", *new(T), *new(R)), and *new(R) for an interface R is a
// nil interface value with no dynamic type, so %T rendered it as the literal
// string "<nil>" regardless of which interface R actually was.
func TestQueryBusKey_InterfaceResultCollapsesToNil(t *testing.T) {
	key1 := queryKey[ifaceQuery, taskView]()
	key2 := queryKey[ifaceQuery, userView]()

	if key1 == key2 {
		t.Fatalf("distinct result types produce the same bus key: %q == %q", key1, key2)
	}
}

// TestQueryBus_SameQueryDifferentInterfaceResults is a regression test for
// GitHub issue #53: registering two handlers for the same query type but
// different interface result types used to collide on one map key, so the
// second registration panicked with ErrDuplicateHandler even though no
// duplicate existed.
func TestQueryBus_SameQueryDifferentInterfaceResults(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (taskView, error) {
		return taskViewImpl{title: "task-" + q.ID_}, nil
	}))

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("registering a second handler for the same query with a "+
					"different interface result type panicked: %v", r)
			}
		}()
		RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (userView, error) {
			return userViewImpl{name: "user-" + q.ID_}, nil
		}))
	}()

	taskGateway := NewQueryGateway[ifaceQuery, taskView](bus)
	userGateway := NewQueryGateway[ifaceQuery, userView](bus)

	got1, err := taskGateway(context.Background(), ifaceQuery{ID_: "1"})
	if err != nil {
		t.Fatalf("taskGateway: unexpected error: %v", err)
	}
	if got1.TaskTitle() != "task-1" {
		t.Errorf("taskGateway = %q, want %q", got1.TaskTitle(), "task-1")
	}

	got2, err := userGateway(context.Background(), ifaceQuery{ID_: "2"})
	if err != nil {
		t.Fatalf("userGateway: unexpected error: %v", err)
	}
	if got2.UserName() != "user-2" {
		t.Errorf("userGateway = %q, want %q", got2.UserName(), "user-2")
	}
}

// TestQueryGateway_InterfaceResultResolvesWrongHandler is a regression test
// for GitHub issue #53, isolating the lookup half of the bug: with only one
// handler registered, for (ifaceQuery, taskView), a gateway built for the
// unregistered pair (ifaceQuery, userView) used to find the taskView handler
// anyway because both interface result types keyed as "<nil>".
func TestQueryGateway_InterfaceResultResolvesWrongHandler(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (taskView, error) {
		return taskViewImpl{title: "task-" + q.ID_}, nil
	}))

	userGateway := NewQueryGateway[ifaceQuery, userView](bus)

	_, err := userGateway(context.Background(), ifaceQuery{ID_: "1"})
	if err == nil {
		t.Fatal("expected an error for an unregistered (query, result) pair")
	}
	if !errors.Is(err, ErrHandlerNotFound) {
		t.Errorf("error = %v, want it to wrap ErrHandlerNotFound; "+
			"the gateway matched a foreign handler because both interface "+
			"result types key as <nil>", err)
	}
}

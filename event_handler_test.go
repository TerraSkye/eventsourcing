package eventsourcing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.Event = (*CartCreated)(nil)
var _ cqrs.Event = (*ItemAdded)(nil)
var _ cqrs.Event = (*OtherEvent)(nil)

type CartCreated struct {
	ID string
}

func (c CartCreated) EventType() string { return cqrs.TypeName(c) }

func (c CartCreated) AggregateID() string { return c.ID }

type ItemAdded struct {
	ID string
}

func (i ItemAdded) AggregateID() string { return i.ID }
func (i ItemAdded) EventType() string   { return cqrs.TypeName(i) }

type OtherEvent struct{}

func (o OtherEvent) AggregateID() string { return "" }
func (o OtherEvent) EventType() string   { return cqrs.TypeName(o) }

// --- Tests ---

type Projector struct{}

func (p Projector) OnItemAdded(ctx context.Context, ev *OtherEvent) error    { return nil }
func (p Projector) OnCartCreated(ctx context.Context, ev *CartCreated) error { return nil }
func (p Projector) OnEvent(ctx context.Context, ev cqrs.Event) error         { return nil }

func TestProjectorExample(t *testing.T) {

	p := Projector{}

	handler1 := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(p.OnCartCreated),
		cqrs.OnEvent(p.OnItemAdded),
	)

	handler2 := cqrs.NewEventHandlerFunc(p.OnEvent)

	handler1.Handle(context.Background(), &CartCreated{ID: "abc"})
	handler2.Handle(context.Background(), &CartCreated{ID: "abc"})
}

func TestTypedEventHandler_Handle_CorrectType(t *testing.T) {
	var called bool
	handler := cqrs.OnEvent(func(ctx context.Context, ev *CartCreated) error {
		called = true
		return nil
	})

	err := handler.Handle(context.Background(), &CartCreated{ID: "abc"})
	assert.NoError(t, err)
	assert.True(t, called, "Handler should have been called")
}

func TestTypedEventHandler_Handle_WrongType(t *testing.T) {
	handler := cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error {
		t.Fail() // should not be called
		return nil
	})

	var skipped *cqrs.ErrSkippedEvent

	err := handler.Handle(context.Background(), ItemAdded{ID: "xyz"})

	if !errors.As(err, &skipped) {
		t.Fatalf("expected skipped event, got %v", err)
	}

}

func TestEventGroupProcessor_RoutesEvents(t *testing.T) {
	calledCart := false
	calledItem := false

	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error {
			calledCart = true
			return nil
		}),
		cqrs.OnEvent(func(ctx context.Context, ev ItemAdded) error {
			calledItem = true
			return nil
		}),
	)

	// Trigger CartCreated
	err := group.Handle(context.Background(), CartCreated{ID: "c1"})
	assert.NoError(t, err)
	assert.True(t, calledCart)
	assert.False(t, calledItem)

	// Trigger ItemAdded
	err = group.Handle(context.Background(), ItemAdded{ID: "i1"})
	assert.NoError(t, err)
	assert.True(t, calledItem)
}

func TestEventGroupProcessor_SkippedEvent(t *testing.T) {
	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
	)

	err := group.Handle(context.Background(), OtherEvent{})

	var expected *cqrs.ErrSkippedEvent

	if !errors.As(err, &expected) {
		t.Fatalf("expected skipped event, got %v", err)
	}
}

func TestEventGroupProcessor_DuplicateHandlerPanics(t *testing.T) {
	assert.Panics(t, func() {
		cqrs.NewEventGroupProcessor(
			cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
			cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
		)
	})
}

func TestEventGroupProcessor_StreamFilter_Sorted(t *testing.T) {
	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev ItemAdded) error { return nil }),
		cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
	)

	names := group.StreamFilter()
	expected := []string{"CartCreated", "ItemAdded"}
	assert.Equal(t, expected, names)
}

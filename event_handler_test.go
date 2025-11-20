package eventsourcing_test

import (
	"context"
	"github.com/stretchr/testify/assert"
	cqrs "github.com/terraskye/eventsourcing"
	"testing"
)

type CartCreated struct {
	ID string
}

func (c CartCreated) AggregateID() string { return c.ID }

type ItemAdded struct {
	ID string
}

func (i ItemAdded) AggregateID() string { return i.ID }

type OtherEvent struct{}

func (o OtherEvent) AggregateID() string { return "" }

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

	handler1.Handle(context.Background(), CartCreated{ID: "abc"})
	handler2.Handle(context.Background(), CartCreated{ID: "abc"})
}

func TestTypedEventHandler_Handle_CorrectType(t *testing.T) {
	var called bool
	handler := cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error {
		called = true
		return nil
	})

	err := handler.Handle(context.Background(), CartCreated{ID: "abc"})
	assert.NoError(t, err)
	assert.True(t, called, "Handler should have been called")
}

func TestTypedEventHandler_Handle_WrongType(t *testing.T) {
	handler := cqrs.OnEvent(func(ctx context.Context, ev CartCreated) error {
		t.Fail() // should not be called
		return nil
	})

	err := handler.Handle(context.Background(), ItemAdded{ID: "xyz"})
	assert.ErrorAs(t, err, &cqrs.ErrSkippedEvent{})
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
	assert.ErrorAs(t, err, &cqrs.ErrSkippedEvent{})
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

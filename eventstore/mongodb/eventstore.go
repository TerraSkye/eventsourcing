package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.EventStore = (*EventStore)(nil)

// EventStore is a MongoDB-backed EventStore that uses the transactional outbox
// pattern. Each Save atomically writes to the events collection (source of
// truth for reads) and the outbox collection (ordered delivery feed for the
// EventBus) inside a single multi-document transaction.
//
// Requirements: MongoDB 4.0+ on a replica set. A standalone mongod started
// with --replSet, or any MongoDB Atlas cluster, satisfies this requirement.
type EventStore struct {
	db       *mongo.Database
	events   *mongo.Collection
	outbox   *mongo.Collection
	counters *mongo.Collection
}

// storedEvent is the BSON document shape in the events collection.
// Payload and Metadata are stored as raw JSON bytes (BSON binary) so that
// event data round-trips faithfully through json.Marshal / json.Unmarshal,
// matching the behaviour of the other adapters.
type storedEvent struct {
	ID             string    `bson:"_id"`
	StreamID       string    `bson:"stream_id"`
	StreamPosition int64     `bson:"stream_position"`
	GlobalPosition int64     `bson:"global_position"`
	EventType      string    `bson:"event_type"`
	Payload        []byte    `bson:"payload"`
	Metadata       []byte    `bson:"metadata"`
	OccurredAt     time.Time `bson:"occurred_at"`
}

// outboxEntry is the BSON document shape in the outbox collection.
// It mirrors storedEvent and is written atomically alongside it in Save.
type outboxEntry struct {
	ID             primitive.ObjectID `bson:"_id"`
	GlobalPosition int64              `bson:"global_position"`
	StreamID       string             `bson:"stream_id"`
	EventID        string             `bson:"event_id"`
	StreamPosition int64              `bson:"stream_position"`
	EventType      string             `bson:"event_type"`
	Payload        []byte             `bson:"payload"`
	Metadata       []byte             `bson:"metadata"`
	OccurredAt     time.Time          `bson:"occurred_at"`
}

type counterDoc struct {
	ID  string `bson:"_id"`
	Seq int64  `bson:"seq"`
}

// NewEventStore creates a new MongoDB-backed EventStore and ensures the
// required indexes exist. It is safe to call multiple times; index creation
// is idempotent.
func NewEventStore(ctx context.Context, db *mongo.Database) (*EventStore, error) {
	s := &EventStore{
		db:       db,
		events:   db.Collection("events"),
		outbox:   db.Collection("outbox"),
		counters: db.Collection("counters"),
	}
	if err := s.ensureIndexes(ctx); err != nil {
		return nil, fmt.Errorf("mongodb eventstore: ensure indexes: %w", err)
	}
	return s, nil
}

func (s *EventStore) ensureIndexes(ctx context.Context) error {
	// Unique compound index on (stream_id, stream_position) for optimistic
	// concurrency control and efficient per-stream reads.
	if _, err := s.events.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "stream_id", Value: 1}, {Key: "stream_position", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("stream_position_unique"),
	}); err != nil {
		return fmt.Errorf("events compound index: %w", err)
	}
	// Index for LoadFromAll (cross-stream global ordering).
	if _, err := s.events.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "global_position", Value: 1}},
		Options: options.Index().SetName("events_global_position"),
	}); err != nil {
		return fmt.Errorf("events global_position index: %w", err)
	}
	// Index for EventBus polling on the outbox collection.
	if _, err := s.outbox.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "global_position", Value: 1}},
		Options: options.Index().SetName("outbox_global_position"),
	}); err != nil {
		return fmt.Errorf("outbox global_position index: %w", err)
	}
	return nil
}

// Save atomically appends all events to both the events and outbox
// collections inside a single MongoDB transaction. The stream state
// constraint is checked first; if the unique index on (stream_id,
// stream_position) catches a concurrent conflict the write is rejected
// with StreamRevisionConflictError.
func (s *EventStore) Save(ctx context.Context, envs []cqrs.Envelope, revision cqrs.StreamState) (cqrs.AppendResult, error) {
	if len(envs) == 0 {
		return cqrs.AppendResult{Successful: true}, nil
	}

	streamID := envs[0].StreamID
	for i, e := range envs {
		if e.StreamID != streamID {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d has different stream ID %q",
				streamID, cqrs.ErrInvalidEventBatch, i, e.StreamID,
			)
		}
	}

	session, err := s.db.Client().StartSession()
	if err != nil {
		return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: start session: %w", streamID, err)
	}
	defer session.EndSession(ctx)

	var txResult cqrs.AppendResult
	_, txErr := session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (any, error) {
		var err error
		txResult, err = s.saveInTransaction(sessCtx, streamID, envs, revision)
		return nil, err
	})
	if txErr != nil {
		return txResult, txErr
	}
	return txResult, nil
}

func (s *EventStore) saveInTransaction(
	ctx mongo.SessionContext,
	streamID string,
	envs []cqrs.Envelope,
	revision cqrs.StreamState,
) (cqrs.AppendResult, error) {
	// Determine the current maximum stream position. A result of 0 means the
	// stream does not yet exist (stream positions start at 1).
	var currentVersion int64
	res := s.events.FindOne(ctx,
		bson.M{"stream_id": streamID},
		options.FindOne().
			SetSort(bson.D{{Key: "stream_position", Value: -1}}).
			SetProjection(bson.M{"stream_position": 1}),
	)
	if res.Err() == nil {
		var doc struct {
			StreamPosition int64 `bson:"stream_position"`
		}
		if err := res.Decode(&doc); err != nil {
			return cqrs.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: decode current version: %w", streamID, err)
		}
		currentVersion = doc.StreamPosition
	} else if !errors.Is(res.Err(), mongo.ErrNoDocuments) {
		return cqrs.AppendResult{StreamID: streamID},
			fmt.Errorf("save events to stream %q: get current version: %w", streamID, res.Err())
	}

	// Enforce the requested stream state.
	switch rev := revision.(type) {
	case cqrs.Any:
		// no constraint
	case cqrs.NoStream:
		if currentVersion != 0 {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: already exists: %w", streamID, cqrs.ErrStreamExists)
		}
	case cqrs.StreamExists:
		if currentVersion == 0 {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: does not exist: %w", streamID, cqrs.ErrStreamNotFound)
		}
	case cqrs.Revision:
		expected := int64(rev)
		if currentVersion != expected {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				&cqrs.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: rev,
					ActualRevision:   cqrs.Revision(uint64(currentVersion)),
				}
		}
	default:
		return cqrs.AppendResult{Successful: false, StreamID: streamID},
			fmt.Errorf("unsupported revision type for stream %q: %w", streamID, cqrs.ErrInvalidRevision)
	}

	// Allocate a contiguous block of global positions within the same
	// transaction so that no gaps can appear in the outbox feed.
	startGlobal, err := s.nextGlobalPositions(ctx, int64(len(envs)))
	if err != nil {
		return cqrs.AppendResult{StreamID: streamID},
			fmt.Errorf("save events to stream %q: allocate global positions: %w", streamID, err)
	}

	nextStreamPos := currentVersion + 1
	for i, e := range envs {
		payload, err := json.Marshal(e.Event)
		if err != nil {
			return cqrs.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: marshal event %d: %w", streamID, i, err)
		}

		meta := e.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return cqrs.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: marshal metadata %d: %w", streamID, i, err)
		}

		occurredAt := e.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now()
		}

		streamPos := nextStreamPos + int64(i)
		globalPos := startGlobal + int64(i)

		eventID := e.EventID
		if eventID == uuid.Nil {
			eventID = uuid.New()
		}
		eventIDStr := eventID.String()

		if _, err := s.events.InsertOne(ctx, storedEvent{
			ID:             eventIDStr,
			StreamID:       streamID,
			StreamPosition: streamPos,
			GlobalPosition: globalPos,
			EventType:      e.Event.EventType(),
			Payload:        payload,
			Metadata:       metaJSON,
			OccurredAt:     occurredAt,
		}); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return cqrs.AppendResult{Successful: false, StreamID: streamID},
					&cqrs.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: revision,
						ActualRevision:   cqrs.Revision(uint64(currentVersion)),
					}
			}
			return cqrs.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: insert event %d: %w", streamID, i, err)
		}

		if _, err := s.outbox.InsertOne(ctx, outboxEntry{
			ID:             primitive.NewObjectID(),
			GlobalPosition: globalPos,
			StreamID:       streamID,
			EventID:        eventIDStr,
			StreamPosition: streamPos,
			EventType:      e.Event.EventType(),
			Payload:        payload,
			Metadata:       metaJSON,
			OccurredAt:     occurredAt,
		}); err != nil {
			return cqrs.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: insert outbox entry %d: %w", streamID, i, err)
		}
	}

	finalVersion := currentVersion + int64(len(envs))
	return cqrs.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: uint64(finalVersion),
	}, nil
}

// nextGlobalPositions atomically allocates count sequential global positions
// using a single-document counter in the counters collection. The counter
// document participates in the surrounding transaction so that a rollback also
// rolls back the allocated range.
// Returns the first position in the allocated block.
func (s *EventStore) nextGlobalPositions(ctx context.Context, count int64) (int64, error) {
	var doc counterDoc
	err := s.counters.FindOneAndUpdate(
		ctx,
		bson.M{"_id": "global_position"},
		bson.M{"$inc": bson.M{"seq": count}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, err
	}
	// doc.Seq is the value after the increment.
	// The allocated range is [doc.Seq - count + 1 … doc.Seq].
	return doc.Seq - count + 1, nil
}

// LoadStream returns all events for the given stream in stream-position order.
func (s *EventStore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	n, err := s.events.CountDocuments(ctx, bson.M{"stream_id": id}, options.Count().SetLimit(1))
	if err != nil {
		return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("load stream %q: %w", id, cqrs.ErrStreamNotFound)
	}
	return s.cursorIterator(ctx,
		bson.M{"stream_id": id},
		bson.D{{Key: "stream_position", Value: 1}},
	)
}

// LoadStreamFrom returns events for a stream starting at the given revision.
func (s *EventStore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	switch rev := version.(type) {
	case cqrs.NoStream:
		n, err := s.events.CountDocuments(ctx, bson.M{"stream_id": id}, options.Count().SetLimit(1))
		if err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if n > 0 {
			return nil, fmt.Errorf("load stream %q: expected empty stream: %w", id, cqrs.ErrStreamExists)
		}
		return cqrs.NewSliceIterator([]*cqrs.Envelope{}), nil

	case cqrs.StreamExists:
		n, err := s.events.CountDocuments(ctx, bson.M{"stream_id": id}, options.Count().SetLimit(1))
		if err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("load stream %q: expected existing stream: %w", id, cqrs.ErrStreamNotFound)
		}
		return s.cursorIterator(ctx,
			bson.M{"stream_id": id},
			bson.D{{Key: "stream_position", Value: 1}},
		)

	case cqrs.Any:
		return s.cursorIterator(ctx,
			bson.M{"stream_id": id},
			bson.D{{Key: "stream_position", Value: 1}},
		)

	default:
		fromPos := rev.ToRawInt64()
		return s.cursorIterator(ctx,
			bson.M{"stream_id": id, "stream_position": bson.M{"$gt": fromPos}},
			bson.D{{Key: "stream_position", Value: 1}},
		)
	}
}

// LoadFromAll returns events across all streams in global-position order,
// starting after the position encoded in version.
func (s *EventStore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	fromPos := version.ToRawInt64()
	return s.cursorIterator(ctx,
		bson.M{"global_position": bson.M{"$gt": fromPos}},
		bson.D{{Key: "global_position", Value: 1}},
	)
}

// Close disconnects the underlying MongoDB client.
func (s *EventStore) Close() error {
	return s.db.Client().Disconnect(context.Background())
}

// cursorIterator opens a Find cursor and returns a lazy Iterator over the
// resulting Envelopes. The cursor is closed once the iterator is exhausted
// or on the first error.
func (s *EventStore) cursorIterator(ctx context.Context, filter bson.M, sort bson.D) (*cqrs.Iterator[*cqrs.Envelope], error) {
	cursor, err := s.events.Find(ctx, filter, options.Find().SetSort(sort))
	if err != nil {
		return nil, err
	}
	return cqrs.NewIteratorFunc(func(ctx context.Context) (*cqrs.Envelope, error) {
		if !cursor.Next(ctx) {
			_ = cursor.Close(ctx)
			if err := cursor.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		var doc storedEvent
		if err := cursor.Decode(&doc); err != nil {
			_ = cursor.Close(ctx)
			return nil, fmt.Errorf("decode event document: %w", err)
		}
		return storedEventToEnvelope(&doc)
	}), nil
}

func storedEventToEnvelope(doc *storedEvent) (*cqrs.Envelope, error) {
	ev, err := cqrs.NewEventByName(doc.EventType)
	if err != nil {
		return nil, fmt.Errorf("cannot create event %q: %w", doc.EventType, err)
	}
	if err := json.Unmarshal(doc.Payload, ev); err != nil {
		return nil, fmt.Errorf("cannot unmarshal event %q: %w", doc.EventType, err)
	}

	var meta map[string]any
	if len(doc.Metadata) > 0 {
		if err := json.Unmarshal(doc.Metadata, &meta); err != nil {
			meta = make(map[string]any)
		}
	} else {
		meta = make(map[string]any)
	}

	eventID, err := uuid.Parse(doc.ID)
	if err != nil {
		return nil, fmt.Errorf("parse event ID %q: %w", doc.ID, err)
	}

	return &cqrs.Envelope{
		EventID:       eventID,
		StreamID:      doc.StreamID,
		Event:         ev,
		Metadata:      meta,
		Version:       uint64(doc.StreamPosition),
		GlobalVersion: uint64(doc.GlobalPosition),
		OccurredAt:    doc.OccurredAt,
	}, nil
}

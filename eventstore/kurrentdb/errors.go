package kurrentdb

import (
	"errors"
	"fmt"
	"io"

	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	cqrs "github.com/terraskye/eventsourcing"
)

// mapError translates err from the KurrentDB client into this package's error
// vocabulary and prefixes it with op for context. It returns nil for a nil
// err.
//
// Where the client's failure has a counterpart among the [cqrs] sentinels,
// the result matches that sentinel under [errors.Is] — a read of a stream
// that does not exist becomes [cqrs.ErrStreamNotFound], for example — so
// callers can handle failures without importing the KurrentDB client or
// knowing its error codes. The client's own error stays in the chain either
// way, so a caller that wants the detail can still reach it with
// [errors.As] and [kurrentdb.Error.Code].
//
// Failures with no cqrs counterpart — authentication, access control, node
// availability, size limits, parsing, internal client and server errors — are
// wrapped and returned as they are, rather than forced into an ill-fitting
// sentinel. A caller that needs to tell those apart must inspect the client
// error.
//
// [io.EOF] is returned unchanged. It is how the client signals a clean end of
// stream, and [cqrs.Iterator] relies on it to end iteration successfully, so
// it must never be decorated or turned into a sentinel.
func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	// Checked before anything else: an end of stream is not a failure, and
	// wrapping it would make Iterator report the read as failed.
	if errors.Is(err, io.EOF) {
		return err
	}
	if sentinel := sentinelFor(err); sentinel != nil {
		return fmt.Errorf("%s: %w: %w", op, sentinel, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// sentinelFor returns the [cqrs] sentinel corresponding to err's KurrentDB
// error code, or nil if there is no faithful counterpart.
//
// A deleted or tombstoned stream maps to [cqrs.ErrStreamNotFound] along with
// a genuinely absent one: from a reader's point of view the events are gone,
// and cqrs has no sentinel for the stronger "gone for good" case. Callers
// that must tell them apart can match the wrapped [kurrentdb.Error] and
// compare its Code against [kurrentdb.ErrorCodeStreamDeleted] or
// [kurrentdb.ErrorCodeStreamTombstoned].
func sentinelFor(err error) error {
	var kErr *kurrentdb.Error
	if !errors.As(err, &kErr) {
		return nil
	}

	switch kErr.Code() {
	case kurrentdb.ErrorCodeResourceNotFound,
		kurrentdb.ErrorCodeStreamDeleted,
		kurrentdb.ErrorCodeStreamTombstoned:
		return cqrs.ErrStreamNotFound

	case kurrentdb.ErrorCodeResourceAlreadyExists:
		return cqrs.ErrStreamExists

	default:
		return nil
	}
}

// isNotFound reports whether err is the client's "stream does not exist"
// failure. The read methods use it to tell an absent stream from a failed
// read, since the two arrive the same way: as an error from the first
// streamer.Recv, not from opening the read.
//
// errors.AsType would express this without the out-parameter, but it needs
// go1.26 and this module targets go1.25 — go vet rejects it as too new. Keep
// errors.As until the go directive moves for a reason of its own.
func isNotFound(err error) bool {
	var kErr *kurrentdb.Error
	return errors.As(err, &kErr) && kErr.IsErrorCode(kurrentdb.ErrorCodeResourceNotFound)
}

// expectationError translates a failed optimistic-concurrency check on Save
// into the error the caller's own expectation implies, so that violating a
// [cqrs.NoStream] or [cqrs.StreamExists] precondition reports the same
// sentinel here as it does in eventstore/memory and eventstore/file. It
// returns nil when err is not a concurrency failure, or when the expectation
// was a [cqrs.Revision].
//
// A Revision mismatch is deliberately left to the caller of this function:
// reporting it faithfully means returning a
// [cqrs.StreamRevisionConflictError], which needs the stream's actual
// revision, and the client only carries that for
// [kurrentdb.ErrorCodeStreamRevisionConflict] — not for the
// [kurrentdb.ErrorCodeWrongExpectedVersion] an ordinary append failure
// produces, where it appears only inside the message text. See
// .bug/eventstore-kurrentdb-save-revision-conflict-not-translated.md.
func expectationError(streamID string, revision cqrs.StreamState, err error) error {
	var kErr *kurrentdb.Error
	if !errors.As(err, &kErr) {
		return nil
	}
	switch kErr.Code() {
	case kurrentdb.ErrorCodeWrongExpectedVersion,
		kurrentdb.ErrorCodeStreamRevisionConflict:
	default:
		return nil
	}

	switch revision.(type) {
	case cqrs.NoStream:
		// The caller required the stream not to exist yet, and it does.
		return fmt.Errorf("save events to stream %q: %w: %w", streamID, cqrs.ErrStreamExists, err)
	case cqrs.StreamExists:
		// The caller required the stream to already exist, and it does not.
		return fmt.Errorf("save events to stream %q: %w: %w", streamID, cqrs.ErrStreamNotFound, err)
	default:
		return nil
	}
}

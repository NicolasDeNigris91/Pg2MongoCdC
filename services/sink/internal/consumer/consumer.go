// Package consumer implements the Kafka→Mongo consume loop with
// commit-after-side-effect semantics (ADR-003): a record's offset is
// committed only AFTER the downstream write has succeeded. On crash
// between write and commit, the record is redelivered, and LSN-gated
// upserts in the writer make the reapplication a no-op.
package consumer

import (
	"context"
	"errors"
	"fmt"

	"zdt/sink/internal/decoder"
	"zdt/sink/internal/writer"
)

// Record is the minimum view of a Kafka record the loop needs.
// Offset is the per-partition position we may commit.
type Record struct {
	Key, Value []byte
	Offset     int64
	Partition  int32
	Topic      string
}

// KafkaConsumer abstracts the Kafka client so tests can substitute a fake.
type KafkaConsumer interface {
	Poll(ctx context.Context) ([]Record, error)
	MarkCommit(r Record)
	CommitMarked(ctx context.Context) error
}

// Writer applies a normalized CDCEvent to the downstream store. An error
// signals retry-worthy failure; the loop must not commit the record.
type Writer interface {
	Apply(ctx context.Context, ev writer.CDCEvent) error
}

// Loop composes a consumer and writer.
type Loop struct {
	Cons         KafkaConsumer
	W            Writer
	SchemaVer    int
	SkipDecodeEr bool // if true, decode errors skip the record (future: route to DLQ). Default false = return error.
}

// RunOnce drains one Poll batch, applies each event to the writer, and
// commits offsets of records whose writes succeeded. Returns the first
// apply error (if any) AFTER CommitMarked, so successful writes prior
// to the error are durably recorded.
func (l *Loop) RunOnce(ctx context.Context) error {
	records, err := l.Cons.Poll(ctx)
	if err != nil {
		return fmt.Errorf("consumer.Loop: poll: %w", err)
	}

	var firstErr error
	for _, r := range records {
		ev, derr := decoder.Decode(r.Key, r.Value)
		if errors.Is(derr, decoder.ErrTombstone) {
			// Tombstone: the preceding "d" event already carried the delete.
			// Mark it committed so the pipeline does not stall on it.
			l.Cons.MarkCommit(r)
			continue
		}
		if derr != nil {
			// Decode error: fail fast. A later cycle will route to DLQ
			// (dlq-send counts as a successful downstream write per ADR-003),
			// at which point this becomes MarkCommit instead of break.
			firstErr = fmt.Errorf("decode offset=%d: %w", r.Offset, derr)
			break
		}
		if werr := l.W.Apply(ctx, ev); werr != nil {
			// Write failed. Do NOT mark — on retry, idempotency absorbs the redeliver.
			firstErr = fmt.Errorf("apply offset=%d lsn=%d: %w", r.Offset, ev.LSN, werr)
			break
		}
		l.Cons.MarkCommit(r)
	}

	// Always flush whatever was marked — successful writes must not be lost
	// to a later record's failure.
	if cerr := l.Cons.CommitMarked(ctx); cerr != nil && firstErr == nil {
		firstErr = fmt.Errorf("commit: %w", cerr)
	}
	return firstErr
}

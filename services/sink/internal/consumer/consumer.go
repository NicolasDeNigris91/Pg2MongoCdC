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
//
// Raw is an opaque handle the underlying KafkaConsumer can stash a
// driver-specific object in (e.g. a *kgo.Record) so it can pass it back
// on MarkCommit without the Loop needing to know the concrete type.
// Tests leave it nil; the franz-go adapter sets it to the source record.
type Record struct {
	Key, Value []byte
	Offset     int64
	Partition  int32
	Topic      string
	Raw        any
}

// KafkaConsumer abstracts the Kafka client so tests can substitute a fake.
type KafkaConsumer interface {
	Poll(ctx context.Context) ([]Record, error)
	MarkCommit(r Record)
	CommitMarked(ctx context.Context) error
}

// Writer applies a batch of normalized CDCEvents to the downstream store.
// An error on any record fails the whole batch; the loop commits nothing,
// and Kafka redelivers every record on next poll. Idempotency at the
// downstream layer (ADR-002's LSN gate) absorbs the duplicates.
//
// Batching is intentional: MongoDB BulkWrite amortizes round-trips, so a
// single Apply call with 500 models is roughly 10x faster than 500 single-
// record calls. Smaller Writer.Apply(ev) convenience wrappers are built on
// top of ApplyBatch for places that still need per-record semantics.
type Writer interface {
	ApplyBatch(ctx context.Context, evs []writer.CDCEvent) error
}

// Loop composes a consumer and writer.
type Loop struct {
	Cons         KafkaConsumer
	W            Writer
	SchemaVer    int
	SkipDecodeEr bool // if true, decode errors skip the record (future: route to DLQ). Default false = return error.
}

// RunOnce drains one Poll batch, decodes every record, dispatches the
// whole set through ApplyBatch, and commits every offset iff the batch
// succeeded. Semantics:
//
//   - Tombstones skip dispatch but still get their offset marked so the
//     pipeline does not stall on them.
//   - Decode error on any record: returns error, commits nothing, whole
//     batch is redelivered on the next poll.
//   - ApplyBatch error: returns error, commits nothing, whole batch is
//     redelivered on the next poll. Idempotency at the downstream layer
//     (ADR-002 LSN gate) absorbs duplicates.
//   - Success path: one BulkWrite to Mongo for the whole batch, then one
//     CommitMarked to Kafka. This is the Week-4 perf fix for the "~240 w/s
//     under burst" gap documented in docs/chaos-findings.md.
func (l *Loop) RunOnce(ctx context.Context) error {
	records, err := l.Cons.Poll(ctx)
	if err != nil {
		return fmt.Errorf("consumer.Loop: poll: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	events := make([]writer.CDCEvent, 0, len(records))
	for _, r := range records {
		ev, derr := decoder.Decode(r.Key, r.Value)
		if errors.Is(derr, decoder.ErrTombstone) {
			continue // sink already materialised the delete; mark at commit time
		}
		if derr != nil {
			// Decode error: fail the whole batch. DLQ routing belongs here in
			// a future cycle - at that point we would mark the record instead.
			return fmt.Errorf("decode offset=%d: %w", r.Offset, derr)
		}
		events = append(events, ev)
	}

	if len(events) > 0 {
		if werr := l.W.ApplyBatch(ctx, events); werr != nil {
			return fmt.Errorf("apply batch (size=%d): %w", len(events), werr)
		}
	}

	// All decoded events were either applied successfully or were tombstones.
	// Safe to mark every record and commit.
	for _, r := range records {
		l.Cons.MarkCommit(r)
	}
	if cerr := l.Cons.CommitMarked(ctx); cerr != nil {
		return fmt.Errorf("commit: %w", cerr)
	}
	return nil
}

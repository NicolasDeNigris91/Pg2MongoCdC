package consumer_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"zdt/sink/internal/consumer"
	"zdt/sink/internal/writer"
)

// --- test doubles ---

type fakeConsumer struct {
	records        []consumer.Record
	markedOffsets  []int64
	commitCalls    int
	committedAfter []int64 // snapshot of markedOffsets at each CommitMarked
}

func (f *fakeConsumer) Poll(ctx context.Context) ([]consumer.Record, error) {
	out := f.records
	f.records = nil
	return out, nil
}

func (f *fakeConsumer) MarkCommit(r consumer.Record) {
	f.markedOffsets = append(f.markedOffsets, r.Offset)
}

func (f *fakeConsumer) CommitMarked(ctx context.Context) error {
	f.commitCalls++
	f.committedAfter = append(f.committedAfter, slices.Clone(f.markedOffsets)...)
	return nil
}

type fakeWriter struct {
	applied   []writer.CDCEvent
	failOnLSN int64 // if >0, Apply returns error when ev.LSN equals this
}

func (f *fakeWriter) Apply(ctx context.Context, ev writer.CDCEvent) error {
	if f.failOnLSN > 0 && ev.LSN == f.failOnLSN {
		return errors.New("synthetic apply failure")
	}
	f.applied = append(f.applied, ev)
	return nil
}

// Fixtures — minimal valid Debezium JSON envelopes.
func makeInsert(pk, lsn int64) (key, value []byte) {
	key = []byte(fmt.Sprintf(`{"payload":{"id":%d}}`, pk))
	value = []byte(fmt.Sprintf(`{"payload":{"before":null,"after":{"id":%d},"source":{"lsn":%d,"table":"users"},"op":"c"}}`, pk, lsn))
	return
}

// --- tests ---

// ADR-003 happy path: every successful apply marks its offset; CommitMarked
// runs once at end of batch.
func TestLoop_AllSuccessfulWritesCommitted(t *testing.T) {
	var recs []consumer.Record
	for i, lsn := range []int64{100, 101, 102} {
		k, v := makeInsert(int64(i+1), lsn)
		recs = append(recs, consumer.Record{Key: k, Value: v, Offset: lsn, Topic: "cdc.users"})
	}
	fc := &fakeConsumer{records: recs}
	fw := &fakeWriter{}
	loop := &consumer.Loop{Cons: fc, W: fw, SchemaVer: 1}

	if err := loop.RunOnce(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []int64{100, 101, 102}; !slices.Equal(fc.markedOffsets, want) {
		t.Errorf("want marked=%v, got %v", want, fc.markedOffsets)
	}
	if fc.commitCalls != 1 {
		t.Errorf("want CommitMarked called once, got %d", fc.commitCalls)
	}
	if len(fw.applied) != 3 {
		t.Errorf("want 3 applied events, got %d", len(fw.applied))
	}
}

// ADR-003 core invariant: if a write fails, that record's offset MUST NOT
// be committed. Records after it in the batch are NOT processed. Earlier
// successful writes ARE committed so their work is preserved across retry.
func TestLoop_FailedWriteStopsBatchAndDoesNotCommitFailedOffset(t *testing.T) {
	recs := []consumer.Record{}
	for i, lsn := range []int64{100, 101, 102} {
		k, v := makeInsert(int64(i+1), lsn)
		recs = append(recs, consumer.Record{Key: k, Value: v, Offset: lsn, Topic: "cdc.users"})
	}
	fc := &fakeConsumer{records: recs}
	fw := &fakeWriter{failOnLSN: 101} // 2nd record fails
	loop := &consumer.Loop{Cons: fc, W: fw, SchemaVer: 1}

	err := loop.RunOnce(context.Background())
	if err == nil {
		t.Fatal("want error, got nil")
	}

	// Only the first record's offset should be marked.
	if slices.Contains(fc.markedOffsets, int64(101)) {
		t.Errorf("offset 101 MUST NOT be marked (its write failed); got %v", fc.markedOffsets)
	}
	if slices.Contains(fc.markedOffsets, int64(102)) {
		t.Errorf("offset 102 MUST NOT be marked (never processed); got %v", fc.markedOffsets)
	}
	if !slices.Contains(fc.markedOffsets, int64(100)) {
		t.Errorf("offset 100 should be marked (its write succeeded); got %v", fc.markedOffsets)
	}

	// CommitMarked must run so offset 100 is durable; redelivery must pick up
	// from 101 on next poll.
	if fc.commitCalls != 1 {
		t.Errorf("want CommitMarked called once even on error, got %d", fc.commitCalls)
	}
}

package writer

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoWriter implements consumer.Writer by dispatching each event to the
// correct Mongo collection via BulkWrite. The heavy lifting (BuildWriteOp +
// ToMongoModel) is unit-tested; this struct is a thin composition tested
// via an integration test in mongo_writer_integration_test.go.
type MongoWriter struct {
	client        *mongo.Client
	db            string
	schemaVersion int
}

// NewMongoWriter returns a writer that upserts into the named database using
// the provided schema version on every write. The client's lifetime is
// managed by the caller (main.go wires connect + disconnect with a context).
func NewMongoWriter(client *mongo.Client, db string, schemaVersion int) *MongoWriter {
	return &MongoWriter{client: client, db: db, schemaVersion: schemaVersion}
}

// Apply is a convenience wrapper around ApplyBatch for callers that still
// want per-record semantics (integration tests, mostly).
func (m *MongoWriter) Apply(ctx context.Context, ev CDCEvent) error {
	return m.ApplyBatch(ctx, []CDCEvent{ev})
}

// ApplyBatch idempotently reflects N CDC events into Mongo in one BulkWrite
// per collection. LSN-gating in each BuildWriteOp filter makes replayed
// events no-ops at the database level (ADR-002).
//
// E11000 (duplicate key) errors from partial-batch upsert failures are
// treated as idempotent no-ops: they signal the stale-replay path where a
// doc already exists with sourceLsn >= ev.LSN and the upsert filter missed.
// The rest of the BulkWrite continues processing; Mongo's BulkWriteException
// carries per-record outcomes so we can tell "all E11000" (OK) apart from
// "real error on at least one record" (return error, whole batch retries).
func (m *MongoWriter) ApplyBatch(ctx context.Context, evs []CDCEvent) error {
	if len(evs) == 0 {
		return nil
	}
	// Group by collection so we issue one BulkWrite per collection.
	byColl := make(map[string][]mongo.WriteModel, 4)
	for _, ev := range evs {
		op, err := BuildWriteOp(ev, m.schemaVersion)
		if err != nil {
			return fmt.Errorf("MongoWriter.ApplyBatch: build op for %s:%s: %w", ev.Table, ev.PK, err)
		}
		byColl[ev.Table] = append(byColl[ev.Table], ToMongoModel(op))
	}

	for table, models := range byColl {
		coll := m.client.Database(m.db).Collection(table)
		// ordered=false so one E11000 doesn't abort the remaining inserts.
		opts := options.BulkWrite().SetOrdered(false)
		if _, err := coll.BulkWrite(ctx, models, opts); err != nil {
			if allDuplicateKey(err) {
				continue // entire "failure" is expected idempotent-skip
			}
			return fmt.Errorf("MongoWriter.ApplyBatch: bulkwrite %s n=%d: %w", table, len(models), err)
		}
	}
	return nil
}

// isDuplicateKey returns true iff err is a BulkWriteException containing at
// least one duplicate-key error (Mongo code 11000). Used by the single-record
// Apply path for backward compatibility.
func isDuplicateKey(err error) bool {
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) {
		for _, we := range bwe.WriteErrors {
			if we.Code == 11000 {
				return true
			}
		}
	}
	return false
}

// allDuplicateKey returns true iff every individual error in a bulk failure
// is E11000. If any non-11000 error is present, the batch is not purely an
// idempotent-skip situation and the caller must retry.
func allDuplicateKey(err error) bool {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) {
		return false
	}
	if len(bwe.WriteErrors) == 0 {
		return false
	}
	for _, we := range bwe.WriteErrors {
		if we.Code != 11000 {
			return false
		}
	}
	return true
}

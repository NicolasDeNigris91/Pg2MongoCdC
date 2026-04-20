package writer

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
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

// Apply idempotently reflects a CDC event into Mongo. LSN-gating in the
// BuildWriteOp filter makes replayed events no-ops at the database level.
//
// Special-case E11000 (duplicate key) from an upsert: this is the stale-
// replay path. The doc exists with sourceLsn >= ev.LSN, so the filter
// does not match any document, Mongo tries to INSERT a new doc with the
// same _id, and the unique index on _id rejects it. Semantically this is
// "already has newer state; redelivered event is redundant" — the exact
// no-op idempotency we want. See ADR-002.
func (m *MongoWriter) Apply(ctx context.Context, ev CDCEvent) error {
	op, err := BuildWriteOp(ev, m.schemaVersion)
	if err != nil {
		return fmt.Errorf("MongoWriter.Apply: build op for %s:%s: %w", ev.Table, ev.PK, err)
	}
	model := ToMongoModel(op)
	coll := m.client.Database(m.db).Collection(ev.Table)
	if _, err := coll.BulkWrite(ctx, []mongo.WriteModel{model}); err != nil {
		if isDuplicateKey(err) {
			return nil
		}
		return fmt.Errorf("MongoWriter.Apply: bulkwrite %s:%s lsn=%d: %w", ev.Table, ev.PK, ev.LSN, err)
	}
	return nil
}

// isDuplicateKey returns true iff err is a BulkWriteException containing at
// least one duplicate-key error (Mongo code 11000).
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

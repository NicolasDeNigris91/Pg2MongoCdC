package writer_test

import (
	"testing"

	"zdt/sink/internal/writer"
)

// Cycle 1: the INSERT path. This test encodes the core ADR-002 invariant:
// every upsert must be LSN-gated so re-delivery of an older event cannot
// overwrite newer state in Mongo.
func TestBuildWriteOp_InsertProducesLSNGatedUpsert(t *testing.T) {
	ev := writer.CDCEvent{
		Table: "users",
		PK:    "42",
		LSN:   1000,
		Op:    writer.OpInsert,
		After: map[string]any{
			"email":     "alice@example.com",
			"full_name": "Alice",
		},
	}

	op, err := writer.BuildWriteOp(ev, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if op.Kind != writer.WriteOpUpsert {
		t.Errorf("want Kind=WriteOpUpsert, got %v", op.Kind)
	}
	if !op.Upsert {
		t.Errorf("want Upsert=true, got false")
	}

	// _id must be the namespaced PK so replays target the same document.
	if got, want := op.Filter["_id"], "users:42"; got != want {
		t.Errorf("want Filter._id=%q, got %v", want, got)
	}

	// LSN gate: $or with a $lt clause and an $exists:false clause.
	orClauses, ok := op.Filter["$or"].([]map[string]any)
	if !ok {
		t.Fatalf("want Filter.$or=[]map[string]any, got %T (%v)", op.Filter["$or"], op.Filter["$or"])
	}
	if len(orClauses) != 2 {
		t.Fatalf("want 2 $or clauses (LSN $lt + $exists:false), got %d", len(orClauses))
	}
	if lt, _ := orClauses[0]["sourceLsn"].(map[string]any); lt["$lt"] != int64(1000) {
		t.Errorf("want $or[0].sourceLsn.$lt=1000, got %v", orClauses[0])
	}
	if ex, _ := orClauses[1]["sourceLsn"].(map[string]any); ex["$exists"] != false {
		t.Errorf("want $or[1].sourceLsn.$exists=false, got %v", orClauses[1])
	}

	// $set must contain the mapped fields plus the LSN and schemaVersion markers.
	set, ok := op.Update["$set"].(map[string]any)
	if !ok {
		t.Fatalf("want Update.$set=map, got %T (%v)", op.Update["$set"], op.Update["$set"])
	}
	if set["sourceLsn"] != int64(1000) {
		t.Errorf("want $set.sourceLsn=1000, got %v", set["sourceLsn"])
	}
	if set["schemaVersion"] != 1 {
		t.Errorf("want $set.schemaVersion=1, got %v", set["schemaVersion"])
	}
	if set["email"] != "alice@example.com" {
		t.Errorf("want $set.email=alice@example.com, got %v", set["email"])
	}
	if set["full_name"] != "Alice" {
		t.Errorf("want $set.full_name=Alice, got %v", set["full_name"])
	}
}

package writer_test

import (
	"testing"

	"zdt/sink/internal/writer"
)

func TestBuildWriteOp_Insert(t *testing.T) {
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

	if got, want := op.Filter["_id"], "users:42"; got != want {
		t.Errorf("want Filter._id=%q, got %v", want, got)
	}

	orClauses, ok := op.Filter["$or"].([]map[string]any)
	if !ok {
		t.Fatalf("want Filter.$or=[]map[string]any, got %T (%v)", op.Filter["$or"], op.Filter["$or"])
	}
	if len(orClauses) != 2 {
		t.Fatalf("want 2 $or clauses, got %d", len(orClauses))
	}
	if lt, _ := orClauses[0]["sourceLsn"].(map[string]any); lt["$lt"] != int64(1000) {
		t.Errorf("want $or[0].sourceLsn.$lt=1000, got %v", orClauses[0])
	}
	if ex, _ := orClauses[1]["sourceLsn"].(map[string]any); ex["$exists"] != false {
		t.Errorf("want $or[1].sourceLsn.$exists=false, got %v", orClauses[1])
	}

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

func TestBuildWriteOp_Delete(t *testing.T) {
	ev := writer.CDCEvent{
		Table: "users",
		PK:    "42",
		LSN:   2000,
		Op:    writer.OpDelete,
		Before: map[string]any{
			"email": "alice@example.com",
		},
	}

	op, err := writer.BuildWriteOp(ev, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if op.Kind != writer.WriteOpDelete {
		t.Errorf("want Kind=WriteOpDelete, got %v", op.Kind)
	}
	if op.Upsert {
		t.Errorf("want Upsert=false, got true")
	}
	if op.Update != nil {
		t.Errorf("want Update=nil, got %v", op.Update)
	}
	if got, want := op.Filter["_id"], "users:42"; got != want {
		t.Errorf("want Filter._id=%q, got %v", want, got)
	}
	lt, _ := op.Filter["sourceLsn"].(map[string]any)
	if lt == nil || lt["$lt"] != int64(2000) {
		t.Errorf("want Filter.sourceLsn.$lt=2000, got %v", op.Filter["sourceLsn"])
	}
	if _, hasOr := op.Filter["$or"]; hasOr {
		t.Errorf("delete filter should not use $or, got %v", op.Filter["$or"])
	}
}

func TestBuildWriteOp_NilAfterErr(t *testing.T) {
	cases := []struct {
		name string
		op   writer.CDCOp
	}{
		{"insert", writer.OpInsert},
		{"update", writer.OpUpdate},
		{"read_snapshot", writer.OpRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := writer.CDCEvent{
				Table: "users",
				PK:    "42",
				LSN:   1000,
				Op:    tc.op,
				After: nil,
			}
			_, err := writer.BuildWriteOp(ev, 1)
			if err == nil {
				t.Fatalf("want error for op=%s with nil After, got nil", tc.op)
			}
		})
	}
}

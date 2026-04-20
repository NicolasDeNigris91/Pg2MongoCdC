// Package writer builds idempotent Mongo write operations from CDC events.
//
// The logic here is pure: it does not touch Mongo, the network, or any state.
// Tests run with `go test` and no external dependencies.
//
// A thin adapter (in a separate file) converts WriteOp into the mongo-driver's
// mongo.WriteModel. Keeping the core logic driver-independent keeps tests fast
// and makes the LSN-gating invariant (ADR-002) readable in isolation.
package writer


// CDCOp is the operation carried by a Debezium event (see the envelope `op` field).
type CDCOp string

const (
	OpInsert CDCOp = "c" // create
	OpUpdate CDCOp = "u"
	OpDelete CDCOp = "d"
	OpRead   CDCOp = "r" // snapshot row
)

// CDCEvent is the minimal cross-source projection of a Debezium event.
// Whatever decoder we use (JSON envelope today, Avro in Week 2 proper)
// normalizes to this shape so BuildWriteOp never sees wire details.
type CDCEvent struct {
	Table  string         // e.g. "users"
	PK     string         // primary key stringified; used for deterministic _id
	LSN    int64          // Postgres WAL position; monotonic per source DB
	Op     CDCOp          // c/u/d/r
	After  map[string]any // row state after the op (nil for deletes)
	Before map[string]any // row state before the op (nil for inserts/reads)
}

// WriteOpKind distinguishes upsert-with-update from delete without leaking
// the mongo driver's internal model types into our pure logic.
type WriteOpKind int

// Ordering matters for test determinism: Delete is 0 so a zero-valued
// WriteOp is visibly "wrong" on assertions for the upsert path.
const (
	WriteOpDelete WriteOpKind = iota
	WriteOpUpsert
)

// WriteOp is a semantic description of a single Mongo write.
// Independent of mongo-go-driver types; a thin adapter converts it.
type WriteOp struct {
	Kind   WriteOpKind
	Filter map[string]any
	Update map[string]any // nil for deletes
	Upsert bool
}

// BuildWriteOp translates a CDCEvent into the Mongo write op that idempotently
// reflects it. The LSN-gate on the filter is what makes at-least-once delivery
// safe: replayed events carry the same (or a smaller) LSN than what's already
// in Mongo, so the filter rejects the write as a no-op. See ADR-002.
//
func BuildWriteOp(ev CDCEvent, schemaVersion int) (WriteOp, error) {
	id := ev.Table + ":" + ev.PK

	if ev.Op == OpDelete {
		return WriteOp{
			Kind: WriteOpDelete,
			Filter: map[string]any{
				"_id":       id,
				"sourceLsn": map[string]any{"$lt": ev.LSN},
			},
		}, nil
	}

	set := make(map[string]any, len(ev.After)+2)
	for k, v := range ev.After {
		set[k] = v
	}
	set["sourceLsn"] = ev.LSN
	set["schemaVersion"] = schemaVersion

	return WriteOp{
		Kind: WriteOpUpsert,
		Filter: map[string]any{
			"_id": id,
			"$or": []map[string]any{
				{"sourceLsn": map[string]any{"$lt": ev.LSN}},
				{"sourceLsn": map[string]any{"$exists": false}},
			},
		},
		Update: map[string]any{"$set": set},
		Upsert: true,
	}, nil
}

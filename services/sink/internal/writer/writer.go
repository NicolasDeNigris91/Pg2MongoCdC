// Package writer builds idempotent Mongo write operations from CDC events.
// The logic is pure: no Mongo, no network. A separate adapter converts
// WriteOp into mongo.WriteModel.
package writer

import "fmt"

type CDCOp string

const (
	OpInsert CDCOp = "c"
	OpUpdate CDCOp = "u"
	OpDelete CDCOp = "d"
	OpRead   CDCOp = "r"
)

// CDCEvent is the minimal cross-source projection of a Debezium event.
type CDCEvent struct {
	Table  string
	PK     string
	LSN    int64
	Op     CDCOp
	After  map[string]any
	Before map[string]any
}

type WriteOpKind int

const (
	WriteOpDelete WriteOpKind = iota
	WriteOpUpsert
)

type WriteOp struct {
	Kind   WriteOpKind
	Filter map[string]any
	Update map[string]any
	Upsert bool
}

// BuildWriteOp builds the Mongo op for one CDC event. The LSN gate on the
// filter is what makes at-least-once delivery safe: a redelivered or stale
// event carries an LSN <= the stored one and is rejected as a no-op.
func BuildWriteOp(ev CDCEvent, schemaVersion int) (WriteOp, error) {
	id := ev.Table + ":" + ev.PK

	if ev.Op != OpDelete && ev.After == nil {
		return WriteOp{}, fmt.Errorf("writer.BuildWriteOp: op=%s requires non-nil After for %s", ev.Op, id)
	}

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

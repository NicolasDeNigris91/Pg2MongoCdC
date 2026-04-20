package writer_test

import (
	"reflect"
	"testing"

	"zdt/sink/internal/writer"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestToMongoModel_UpsertBecomesUpdateOneModel(t *testing.T) {
	op := writer.WriteOp{
		Kind: writer.WriteOpUpsert,
		Filter: map[string]any{
			"_id": "users:42",
			"$or": []map[string]any{
				{"sourceLsn": map[string]any{"$lt": int64(1000)}},
				{"sourceLsn": map[string]any{"$exists": false}},
			},
		},
		Update: map[string]any{"$set": map[string]any{
			"email":         "alice@example.com",
			"sourceLsn":     int64(1000),
			"schemaVersion": 1,
		}},
		Upsert: true,
	}

	m := writer.ToMongoModel(op)
	uom, ok := m.(*mongo.UpdateOneModel)
	if !ok {
		t.Fatalf("want *UpdateOneModel, got %T", m)
	}
	if !reflect.DeepEqual(uom.Filter, op.Filter) {
		t.Errorf("Filter mismatch:\n want %v\n got  %v", op.Filter, uom.Filter)
	}
	if !reflect.DeepEqual(uom.Update, op.Update) {
		t.Errorf("Update mismatch:\n want %v\n got  %v", op.Update, uom.Update)
	}
	if uom.Upsert == nil || *uom.Upsert != true {
		t.Errorf("want Upsert=*true, got %v", uom.Upsert)
	}
}

func TestToMongoModel_DeleteBecomesDeleteOneModel(t *testing.T) {
	op := writer.WriteOp{
		Kind: writer.WriteOpDelete,
		Filter: map[string]any{
			"_id":       "users:42",
			"sourceLsn": map[string]any{"$lt": int64(2000)},
		},
	}

	m := writer.ToMongoModel(op)
	dom, ok := m.(*mongo.DeleteOneModel)
	if !ok {
		t.Fatalf("want *DeleteOneModel, got %T", m)
	}
	if !reflect.DeepEqual(dom.Filter, op.Filter) {
		t.Errorf("Filter mismatch:\n want %v\n got  %v", op.Filter, dom.Filter)
	}
}

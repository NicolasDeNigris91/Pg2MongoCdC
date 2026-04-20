package writer

import "go.mongodb.org/mongo-driver/v2/mongo"

// ToMongoModel converts a driver-independent WriteOp into a mongo-driver
// WriteModel suitable for BulkWrite. Kept in a separate file from BuildWriteOp
// so the pure-logic layer stays importable without the mongo driver.
//
func ToMongoModel(op WriteOp) mongo.WriteModel {
	if op.Kind == WriteOpDelete {
		return mongo.NewDeleteOneModel().SetFilter(op.Filter)
	}
	return mongo.NewUpdateOneModel().
		SetFilter(op.Filter).
		SetUpdate(op.Update).
		SetUpsert(op.Upsert)
}

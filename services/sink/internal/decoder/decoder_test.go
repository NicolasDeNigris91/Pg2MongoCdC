package decoder_test

import (
	"errors"
	"testing"

	"zdt/sink/internal/decoder"
	"zdt/sink/internal/writer"
)

// Cycle 5: INSERT decoding. Exercises the payload.op="c" path, extracts PK
// from the key, LSN from payload.source.lsn, table from payload.source.table,
// and the full After map. Before must be nil for an insert.
func TestDecode_Insert(t *testing.T) {
	// What Debezium actually emits on our stack - peeked from cdc.users with
	// kafka-console-consumer during Week 1 verification. Shrunk to essentials.
	key := []byte(`{"schema":{},"payload":{"id":42}}`)
	value := []byte(`{
		"schema":{},
		"payload":{
			"before":null,
			"after":{"id":42,"email":"alice@example.com","full_name":"Alice"},
			"source":{"version":"2.6","lsn":1000,"table":"users","ts_ms":123,"db":"app"},
			"op":"c",
			"ts_ms":456
		}
	}`)

	ev, err := decoder.Decode(key, value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ev.Table != "users" {
		t.Errorf("want Table=users, got %q", ev.Table)
	}
	if ev.PK != "42" {
		t.Errorf("want PK=42, got %q", ev.PK)
	}
	if ev.LSN != 1000 {
		t.Errorf("want LSN=1000, got %d", ev.LSN)
	}
	if ev.Op != writer.OpInsert {
		t.Errorf("want Op=c, got %q", ev.Op)
	}
	if ev.Before != nil {
		t.Errorf("want Before=nil, got %v", ev.Before)
	}
	if ev.After == nil || ev.After["email"] != "alice@example.com" {
		t.Errorf("want After.email=alice@example.com, got %v", ev.After)
	}
}

func TestDecode_Tombstone(t *testing.T) {
	key := []byte(`{"schema":{},"payload":{"id":42}}`)
	value := []byte(nil) // tombstone

	_, err := decoder.Decode(key, value)
	if !errors.Is(err, decoder.ErrTombstone) {
		t.Errorf("want ErrTombstone, got %v", err)
	}
}

func TestDecode_Delete(t *testing.T) {
	key := []byte(`{"schema":{},"payload":{"id":42}}`)
	value := []byte(`{
		"schema":{},
		"payload":{
			"before":{"id":42,"email":"alice@example.com","full_name":"Alice"},
			"after":null,
			"source":{"version":"2.6","lsn":2000,"table":"users"},
			"op":"d"
		}
	}`)

	ev, err := decoder.Decode(key, value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Op != writer.OpDelete {
		t.Errorf("want Op=d, got %q", ev.Op)
	}
	if ev.LSN != 2000 {
		t.Errorf("want LSN=2000, got %d", ev.LSN)
	}
	if ev.After != nil {
		t.Errorf("want After=nil, got %v", ev.After)
	}
	if ev.Before == nil || ev.Before["email"] != "alice@example.com" {
		t.Errorf("want Before.email=alice@example.com, got %v", ev.Before)
	}
}

func TestDecode_MalformedJSONIsError(t *testing.T) {
	key := []byte(`{"payload":{"id":42}}`)
	value := []byte(`{not json`)
	_, err := decoder.Decode(key, value)
	if err == nil {
		t.Fatalf("want error on malformed JSON, got nil")
	}
}

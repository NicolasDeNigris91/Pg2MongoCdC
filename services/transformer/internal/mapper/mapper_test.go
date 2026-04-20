package mapper_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"transformer/internal/mapper"
)

func writeRuleFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyJSON_RenamesFieldsInAfterAndBefore(t *testing.T) {
	dir := t.TempDir()
	writeRuleFile(t, dir, "users.yml", `
source: public.users
target: users
fields:
  full_name: { type: string, target: fullName }
  created_at: { type: timestamptz, target: createdAt }
`)

	m, err := mapper.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	in := []byte(`{
		"payload": {
			"before": {"id": 1, "full_name": "Old"},
			"after":  {"id": 1, "full_name": "New", "email": "a@b.c", "created_at": "2026-01-01"},
			"source": {"lsn": 100, "table": "users"},
			"op": "u"
		}
	}`)

	out, err := m.ApplyJSON("cdc.users", in)
	if err != nil {
		t.Fatalf("ApplyJSON: %v", err)
	}

	var env map[string]any
	_ = json.Unmarshal(out, &env)
	payload := env["payload"].(map[string]any)

	after := payload["after"].(map[string]any)
	if _, bad := after["full_name"]; bad {
		t.Errorf("after still has full_name: %v", after)
	}
	if got := after["fullName"]; got != "New" {
		t.Errorf("after.fullName: want 'New', got %v", got)
	}
	if got := after["createdAt"]; got != "2026-01-01" {
		t.Errorf("after.createdAt: want date, got %v", got)
	}
	// Fields without a rule entry pass through unchanged.
	if got := after["email"]; got != "a@b.c" {
		t.Errorf("after.email passthrough: got %v", got)
	}

	before := payload["before"].(map[string]any)
	if _, bad := before["full_name"]; bad {
		t.Errorf("before still has full_name: %v", before)
	}
	if got := before["fullName"]; got != "Old" {
		t.Errorf("before.fullName: got %v", got)
	}
}

func TestApplyJSON_NoRuleForTopicPassesThrough(t *testing.T) {
	m, err := mapper.Load(t.TempDir()) // empty rules dir
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(`{"payload":{"after":{"id":1,"full_name":"X"}}}`)
	out, _ := m.ApplyJSON("cdc.orders", in)
	// Bytes may differ (re-serialization is not attempted when no rule), but
	// semantic content must match.
	if string(out) != string(in) {
		t.Errorf("pass-through expected, got mutation:\n want %s\n got  %s", in, out)
	}
}

func TestApplyJSON_TombstoneOrNonEnvelopePassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeRuleFile(t, dir, "users.yml", `
source: public.users
target: users
fields:
  full_name: { target: fullName }
`)
	m, _ := mapper.Load(dir)

	out, err := m.ApplyJSON("cdc.users", []byte(`null`))
	if err != nil {
		t.Fatalf("unexpected error for null payload: %v", err)
	}
	if string(out) != "null" {
		t.Errorf("want null passed through, got %s", out)
	}
}

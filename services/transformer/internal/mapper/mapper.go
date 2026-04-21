// Package mapper reads declarative YAML transform rules and applies them to
// Debezium-envelope JSON events. The output is the same envelope shape with
// field names rewritten per the rule so the downstream sink can write
// documents in their target Mongo shape without per-table code.
//
// This is ADR-004 ("configuration over code") in code: adding a new table
// to the pipeline is a new file under schema/transforms/, not a deploy.
package mapper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldRule is the YAML entry for one column's rewrite.
type FieldRule struct {
	Type   string `yaml:"type"`
	Target string `yaml:"target"`
}

// Rule is the parsed form of one YAML file.
type Rule struct {
	Source string               `yaml:"source"` // e.g. "public.users"
	Target string               `yaml:"target"` // e.g. "users"
	Fields map[string]FieldRule `yaml:"fields"`
}

// Mapper holds all rules indexed by the Kafka topic "table" suffix (e.g.
// "users" for the topic "cdc.users", extracted from Rule.Source).
type Mapper struct {
	byTable map[string]*Rule
}

// Load parses every *.yml under dir. Unknown YAML keys are tolerated so
// future rule additions do not require code changes here.
func Load(dir string) (*Mapper, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("mapper.Load: read dir %s: %w", dir, err)
	}
	m := &Mapper{byTable: map[string]*Rule{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// #nosec G304 -- dir is an operator-provided config root (RULES_DIR
		// env var), e.Name() comes from os.ReadDir(dir) of that same root
		// and is filtered to *.yml. No user input reaches this path.
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("mapper.Load: %s: %w", path, err)
		}
		var r Rule
		if err := yaml.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("mapper.Load: %s: %w", path, err)
		}
		table := tableFromSource(r.Source)
		if table == "" {
			return nil, fmt.Errorf("mapper.Load: %s: empty source", path)
		}
		m.byTable[table] = &r
	}
	return m, nil
}

// ApplyJSON takes a topic name (e.g. "cdc.users") plus the raw Debezium
// envelope bytes and returns the envelope with payload.after/before field
// names rewritten per the matching rule. Events whose topic has no matching
// rule are passed through unchanged.
func (m *Mapper) ApplyJSON(topic string, envelope []byte) ([]byte, error) {
	table := tableFromTopic(topic)
	rule, ok := m.byTable[table]
	if !ok {
		return envelope, nil
	}

	var env map[string]any
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("mapper.ApplyJSON: parse: %w", err)
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok {
		return envelope, nil // tombstone or non-envelope; leave alone
	}

	if after, ok := payload["after"].(map[string]any); ok && after != nil {
		payload["after"] = renameKeys(after, rule.Fields)
	}
	if before, ok := payload["before"].(map[string]any); ok && before != nil {
		payload["before"] = renameKeys(before, rule.Fields)
	}
	env["payload"] = payload

	return json.Marshal(env)
}

// Rules is the read-only view used by tests and /debug.
func (m *Mapper) Rules() map[string]*Rule { return m.byTable }

// tableFromSource extracts the last segment after '.'. "public.users" -> "users".
func tableFromSource(src string) string {
	if i := strings.LastIndex(src, "."); i >= 0 {
		return src[i+1:]
	}
	return src
}

// tableFromTopic takes "cdc.users" or "transformed.users" and returns "users".
func tableFromTopic(topic string) string {
	if i := strings.LastIndex(topic, "."); i >= 0 {
		return topic[i+1:]
	}
	return topic
}

func renameKeys(src map[string]any, fields map[string]FieldRule) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		target := k
		if r, ok := fields[k]; ok && r.Target != "" {
			target = r.Target
		}
		out[target] = v
	}
	return out
}

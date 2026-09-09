package web

import "testing"

func testTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object", "required": []any{"city"}, "properties": map[string]any{"city": map[string]any{"type": "string"}}}}},
		{"type": "function", "function": map[string]any{"name": "get_time", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}}},
	}
}

func TestSchemaValidNestedAndEnum(t *testing.T) {
	fn := map[string]any{"parameters": map[string]any{"type": "object", "required": []any{"query"}, "additionalProperties": false, "properties": map[string]any{"query": map[string]any{"type": "object", "required": []any{"city"}, "properties": map[string]any{"city": map[string]any{"type": "string"}, "unit": map[string]any{"type": "string", "enum": []any{"c", "f"}}}}, "days": map[string]any{"type": "integer"}}}}
	if err := schemaValid(map[string]any{"query": map[string]any{"city": "Paris", "unit": "c"}, "days": float64(2)}, fn); err != nil {
		t.Fatal(err)
	}
	if err := schemaValid(map[string]any{"query": map[string]any{"city": "Paris", "unit": "kelvin"}}, fn); err == nil {
		t.Fatal("expected enum rejection")
	}
	if err := schemaValid(map[string]any{"query": map[string]any{"city": "Paris"}, "extra": true}, fn); err == nil {
		t.Fatal("expected additional property rejection")
	}
	if err := schemaValid(map[string]any{"query": map[string]any{"city": "Paris"}, "days": 1.5}, fn); err == nil {
		t.Fatal("expected integer rejection")
	}
}

// Regression: the router model occasionally emits params that exist on a
// sibling tool but not on the chosen one (e.g. offset/limit on Read). Dropping
// the whole call on the untouched keys wasted a router round and silently
// turned the decision into prose. Extra undeclared keys must be stripped and
// the known fields still executed; genuine type/required failures must still
// be rejected.
func TestCoerceToolArgsStripsUndeclaredKeys(t *testing.T) {
	fn := map[string]any{"parameters": map[string]any{"type": "object", "required": []any{"city"}, "additionalProperties": false, "properties": map[string]any{"city": map[string]any{"type": "string"}}}}

	got, dropped := coerceToolArgs(map[string]any{"city": "Beijing", "offset": float64(1783), "limit": float64(90)}, fn)
	if !dropped {
		t.Fatal("expected undeclared keys to be reported as dropped")
	}
	if got == nil {
		t.Fatal("expected args to be salvageable")
	}
	if len(got) != 1 {
		t.Fatalf("expected only the declared key to survive: %v", got)
	}
	if _, ok := got["offset"]; ok {
		t.Fatalf("undeclared offset must be stripped: %v", got)
	}

	// Failure modes beyond extra keys must NOT be silently swallowed.
	if got, _ := coerceToolArgs(map[string]any{"city": float64(2)}, fn); got != nil {
		t.Fatalf("type mismatch must still be rejected: %v", got)
	}
	if got, _ := coerceToolArgs(map[string]any{"offset": float64(1)}, fn); got != nil {
		t.Fatalf("missing required + extra keys must still be rejected: %v", got)
	}
}

package web

import (
	"encoding/json"
	"fmt"
	"math"
)

func toolFunction(name string, tools []map[string]any) map[string]any {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			return f
		}
	}
	return nil
}

func schemaValid(args map[string]any, fn map[string]any) error {
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return nil
	}
	return validateJSONSchema(args, params, "arguments")
}

// coerceToolArgs returns the arguments to execute for a tool call, tolerating
// extra keys the schema does not declare. The router model occasionally emits
// params that exist on a sibling tool but not on the chosen one (e.g. offset/
// limit on Read); dropping the whole call on additionalProperties cost a full
// router round and silently turned the decision into prose. Mirrors new-api's
// pass-through principle: keep required/type checks, but when the only failure
// is undeclared keys, strip them and execute with what the schema knows. The
// second return value reports whether undeclared keys were dropped.
func coerceToolArgs(args map[string]any, fn map[string]any) (map[string]any, bool) {
	if schemaValid(args, fn) == nil {
		return args, false
	}
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return args, false
	}
	props, _ := params["properties"].(map[string]any)
	if props == nil {
		return nil, false
	}
	stripped := make(map[string]any, len(args))
	dropped := false
	for k, v := range args {
		if _, ok := props[k]; ok {
			stripped[k] = v
		} else {
			dropped = true
		}
	}
	if schemaValid(stripped, fn) != nil {
		return nil, false
	}
	return stripped, dropped
}

func validateJSONSchema(value any, schema map[string]any, path string) error {
	if enums, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			a, _ := json.Marshal(value)
			b, _ := json.Marshal(e)
			if string(a) == string(b) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is not an allowed enum value", path)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		if req, ok := schema["required"].([]any); ok {
			for _, raw := range req {
				n, _ := raw.(string)
				if _, yes := m[n]; !yes {
					return fmt.Errorf("missing required argument %s", n)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
			for n := range m {
				if _, yes := props[n]; !yes {
					return fmt.Errorf("%s.%s is not allowed", path, n)
				}
			}
		}
		for n, v := range m {
			if ps, ok := props[n].(map[string]any); ok {
				if err := validateJSONSchema(v, ps, path+"."+n); err != nil {
					return err
				}
			}
		}
	case "array":
		a, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		if item, ok := schema["items"].(map[string]any); ok {
			for i, v := range a {
				if err := validateJSONSchema(v, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || math.Trunc(n) != n {
			return fmt.Errorf("%s must be integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}

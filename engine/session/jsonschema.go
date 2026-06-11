package session

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ValidateJSON validates a JSON payload against a DELIBERATE SUBSET of JSON Schema.
// It is the SINGLE validation choke point for model-authored structured output (the
// Subagent tool's output_schema): the child calls a synthetic SubmitResult tool whose
// parameters ARE the schema, and the Subagent tool validates the submitted payload here
// before accepting it as the delegation's deliverable.
//
// It is a DOMAIN helper (it validates the shape of arbitrary model output, a domain
// concern) and is DISTINCT from ValidateMediaParts (which validates prompt MEDIA, a
// different concern with a different shape) — the two never share a code path, so
// this is not a second media-validation path.
//
// SUPPORTED keywords (the subset):
//   - "type": one of object | array | string | number | integer | boolean | null
//     (also accepts a []string union, satisfied if ANY listed type matches).
//   - "properties" (object): per-key sub-schemas, validated recursively.
//   - "required" (object): listed keys must be present.
//   - "items" (array): a single sub-schema applied to every element.
//   - "enum": the value must deep-equal one of the listed JSON values.
//
// FAIL-OPEN policy (the scope-creep guard): any keyword NOT in the subset is
// IGNORED, never an error — an unsupported construct degrades to "any JSON here",
// it never hard-fails. A schema that is not a JSON object at all (e.g. `true`) also
// validates everything. This keeps the validator a small, predictable subset and
// resists the documented scope-creep-into-a-full-validator risk; the cost is that an
// exotic schema is under-enforced, which is acceptable for a coarse deliverable gate.
//
// It returns a single, model-readable error describing the FIRST mismatch (path +
// expectation), or nil when the payload satisfies the subset.
func ValidateJSON(schema, payload json.RawMessage) error {
	var sch map[string]json.RawMessage
	if err := json.Unmarshal(schema, &sch); err != nil {
		// A non-object schema (or malformed): fail-open — validate everything.
		return nil
	}
	var val any
	if err := json.Unmarshal(payload, &val); err != nil {
		return fmt.Errorf("payload is not valid JSON: %v", err)
	}
	return validateValue("", sch, val)
}

// validateValue applies the subset rules of one (object) schema node to one decoded
// JSON value at the given path ("" = root). It validates type, then enum, then the
// object/array structural keywords; an unrecognised keyword is silently ignored
// (fail-open).
func validateValue(path string, sch map[string]json.RawMessage, val any) error {
	if raw, ok := sch["type"]; ok {
		if err := validateType(path, raw, val); err != nil {
			return err
		}
	}
	if raw, ok := sch["enum"]; ok {
		if err := validateEnum(path, raw, val); err != nil {
			return err
		}
	}
	if raw, ok := sch["properties"]; ok {
		if err := validateProperties(path, raw, val); err != nil {
			return err
		}
	}
	if raw, ok := sch["required"]; ok {
		if err := validateRequired(path, raw, val); err != nil {
			return err
		}
	}
	if raw, ok := sch["items"]; ok {
		if err := validateItems(path, raw, val); err != nil {
			return err
		}
	}
	return nil
}

// validateType checks the JSON value against a "type" that is either a single string
// or a []string union (satisfied if ANY listed type matches). An unrecognised type
// string is treated as "any" (fail-open).
func validateType(path string, raw json.RawMessage, val any) error {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if !jsonTypeMatches(single, val) {
			return typeError(path, []string{single}, val)
		}
		return nil
	}
	var union []string
	if json.Unmarshal(raw, &union) == nil && len(union) > 0 {
		for _, t := range union {
			if jsonTypeMatches(t, val) {
				return nil
			}
		}
		return typeError(path, union, val)
	}
	// Unparseable type clause: fail-open.
	return nil
}

// jsonTypeMatches reports whether the decoded JSON value matches a JSON Schema type
// name. "integer" requires a whole number; "number" accepts any JSON number. An
// unknown type name matches anything (fail-open).
func jsonTypeMatches(t string, val any) bool {
	switch t {
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number":
		_, ok := val.(float64)
		return ok
	case "integer":
		f, ok := val.(float64)
		return ok && f == math.Trunc(f)
	case "null":
		return val == nil
	default:
		return true // unknown type name: fail-open.
	}
}

// validateEnum checks the value deep-equals one of the enumerated JSON values.
func validateEnum(path string, raw json.RawMessage, val any) error {
	var options []any
	if json.Unmarshal(raw, &options) != nil {
		return nil // malformed enum: fail-open.
	}
	for _, opt := range options {
		if jsonDeepEqual(opt, val) {
			return nil
		}
	}
	return fmt.Errorf("%s: value is not one of the allowed enum values", fieldLabel(path))
}

// validateProperties validates each declared property's sub-schema against the
// matching member of an object value. A non-object value is ignored here (the "type"
// keyword, if present, already reported it; if absent, properties on a non-object are
// vacuously satisfied — fail-open).
func validateProperties(path string, raw json.RawMessage, val any) error {
	obj, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(raw, &props) != nil {
		return nil
	}
	// Deterministic order so the FIRST reported mismatch is stable across runs.
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		member, present := obj[key]
		if !present {
			continue // presence is "required"'s job, not "properties".
		}
		var subSch map[string]json.RawMessage
		if json.Unmarshal(props[key], &subSch) != nil {
			continue // non-object sub-schema: fail-open.
		}
		if err := validateValue(joinPath(path, key), subSch, member); err != nil {
			return err
		}
	}
	return nil
}

// validateRequired checks every listed key is present on an object value.
func validateRequired(path string, raw json.RawMessage, val any) error {
	obj, ok := val.(map[string]any)
	if !ok {
		return nil // required on a non-object: type keyword owns the mismatch.
	}
	var required []string
	if json.Unmarshal(raw, &required) != nil {
		return nil
	}
	for _, key := range required {
		if _, present := obj[key]; !present {
			return fmt.Errorf("%s: missing required property %q", fieldLabel(path), key)
		}
	}
	return nil
}

// validateItems applies a single sub-schema to every element of an array value.
func validateItems(path string, raw json.RawMessage, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return nil
	}
	var subSch map[string]json.RawMessage
	if json.Unmarshal(raw, &subSch) != nil {
		return nil // items is not a single object sub-schema (e.g. tuple form): fail-open.
	}
	for i, elem := range arr {
		if err := validateValue(fmt.Sprintf("%s[%d]", pathOrRoot(path), i), subSch, elem); err != nil {
			return err
		}
	}
	return nil
}

// jsonDeepEqual compares two decoded JSON values for structural equality, used by
// enum matching. It re-marshals both and compares the canonical bytes, which is exact
// for the JSON value space (numbers, strings, bools, null, arrays, objects).
func jsonDeepEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// typeError builds the model-readable type-mismatch error.
func typeError(path string, want []string, got any) error {
	return fmt.Errorf("%s: expected type %s but got %s",
		fieldLabel(path), strings.Join(want, " or "), jsonTypeName(got))
}

// jsonTypeName names the JSON type of a decoded value for error messages.
func jsonTypeName(v any) string {
	switch t := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if t == math.Trunc(t) {
			return "integer"
		}
		return "number"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

// fieldLabel renders a path for an error message ("the root value" for "").
func fieldLabel(path string) string {
	if path == "" {
		return "the root value"
	}
	return "property " + path
}

// pathOrRoot renders a path for indexed array messages ("$" for the root).
func pathOrRoot(path string) string {
	if path == "" {
		return "$"
	}
	return path
}

// joinPath appends a property key to a dotted path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

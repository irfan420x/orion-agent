// Package tools — central schema validation for tool arguments.
//
// Registry.Execute validates every call against the tool's own
// Definition before the tool runs: required parameters must be
// present, and every supplied parameter must match its declared JSON
// type (string, integer, number, boolean, object, array) and, when
// the schema declares one, its enum and its numeric/string bounds
// (minimum, maximum, minLength, maxLength, pattern). Validation
// recurses into nested object properties/required and array items,
// so a nested schema (e.g. todo_write's todos[].content/status) is
// enforced here, not only at the top level. A validation failure is
// a clean invalid_args ToolError — never a panic, never a
// half-executed tool — and validation runs inside Execute, after the
// agent's permission gate has already decided the call (the gate runs
// in core before Execute is reached) and before any tool code,
// denylist check, or side effect. A call that fails validation
// therefore cannot bypass the gate or the denylist: it never reaches
// the tool at all.
//
// Schemas are the definitions tools already publish to the model
// (model.ToolDefinition.Parameters, OpenAI-style JSON Schema), so
// there is no second schema to drift: a tool that declares a
// parameter as required or typed gets that enforced for every caller
// — the model, the CLI, and direct registry users alike.
package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"

	"github.com/irfan/orion/internal/model"
)

// validateArgs checks args against def's Parameters schema. An empty
// or null args value means "no arguments" ({}), matching
// core.parseToolArguments. A non-object top level is rejected. A nil
// Parameters (or one with no usable "properties"/"required") imposes
// no constraint beyond the top-level object check: a tool that
// publishes no schema cannot have its arguments second-guessed here,
// and a malformed schema must never turn a valid call into an error.
//
// The returned error is always a *ToolError with Kind "invalid_args"
// (or nil when the call is valid).
func validateArgs(def model.ToolDefinition, args json.RawMessage) error {
	tool := def.Name
	invalid := func(format string, a ...any) error {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf(format, a...)}
	}

	// Decode the top level. Empty / whitespace / null means {}.
	// json.Unmarshal is the authority: it rejects trailing data
	// after the first JSON value outright.
	trimmed := strings.TrimSpace(string(args))
	var obj map[string]any
	switch {
	case trimmed == "" || trimmed == "null":
		obj = map[string]any{}
	default:
		var v any
		if err := json.Unmarshal(args, &v); err != nil {
			return invalid("arguments must be a JSON object: invalid JSON: %v", err)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return invalid("arguments must be a JSON object, got %s", jsonTypeName(v))
		}
		obj = m
	}

	if len(def.Parameters) == 0 {
		return nil
	}
	return validateObject(tool, "", obj, def.Parameters)
}

// validateObject checks obj against an object schema: required
// parameters must be present, and every supplied property matching a
// declared property schema is validated (recursively, via
// validateValue). name is the dotted path of obj in the arguments
// ("" at the top level) used to name nested parameters in errors:
// a missing field inside todos[0] is reported as
// "todos[0].content", not as a bare "content". A schema with no
// usable "properties"/"required" imposes no constraint: a malformed
// schema must never turn a valid call into an error.
func validateObject(tool, name string, obj map[string]any, schema map[string]any) error {
	invalid := func(format string, a ...any) error {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf(format, a...)}
	}
	props, _ := schema["properties"].(map[string]any)
	required := toStringSlice(schema["required"])

	for _, req := range required {
		if req == "" {
			continue
		}
		if _, ok := obj[req]; !ok {
			return invalid("missing required parameter %q", joinName(name, req))
		}
	}
	requiredSet := make(map[string]bool, len(required))
	for _, req := range required {
		requiredSet[req] = true
	}
	for propName, val := range obj {
		if val == nil && !requiredSet[propName] {
			// JSON null for an OPTIONAL parameter means
			// "absent": providers send null for fields they
			// do not fill, and rejecting that would break
			// otherwise-valid calls. A REQUIRED parameter
			// that is present-but-null still fails its type
			// check below (validateValue).
			continue
		}
		prop, ok := props[propName]
		if !ok {
			// Unknown parameters are ignored: providers add
			// extra fields, and rejecting them would break
			// calls that are otherwise valid.
			continue
		}
		propSchema, ok := prop.(map[string]any)
		if !ok {
			continue // malformed property schema: never fail the call
		}
		if err := validateValue(tool, joinName(name, propName), val, propSchema); err != nil {
			return err
		}
	}
	return nil
}

// joinName appends a property name to a dotted parameter path (""
// at the top level yields the bare name).
func joinName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// validateValue checks one supplied parameter against its property
// schema (type, enum, numeric/string bounds, and nested
// properties/required/items). A JSON null is treated as "absent" for
// an optional parameter — providers send null for fields they do not
// fill — but a required parameter that is present-but-null still
// fails its type check unless the schema allows "null". Nested
// schemas are enforced recursively: an object value is checked
// against the schema's own properties/required, and each array
// element against the schema's items (Audit 5 F-A5-P2-27 — the old
// code validated only the top level, so e.g. todo_write's
// todos[].content/status were never checked here).
func validateValue(tool, name string, val any, schema map[string]any) error {
	invalid := func(format string, a ...any) error {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf(format, a...)}
	}
	types := schemaTypeNames(schema["type"])
	if val == nil {
		if len(types) == 0 || containsType(types, "null") {
			return nil
		}
		return invalid("parameter %q must be %s, got null", name, describeTypes(types))
	}
	if len(types) > 0 {
		matched := false
		for _, t := range types {
			if valueMatchesType(val, t) {
				matched = true
				break
			}
		}
		if !matched {
			return invalid("parameter %q must be %s, got %s", name, describeTypes(types), jsonTypeName(val))
		}
	}
	// Enum membership is one constraint among others, not a pass:
	// a value that matches the enum must still satisfy the type,
	// bounds, pattern, and nested checks below. The old code
	// returned success the moment a value matched an enum entry,
	// so a schema with enum ["a","b"] + pattern "^[0-9]+$"
	// accepted "a" (Audit 6 F-A6-P2-11) and an enum on a number
	// skipped its minimum/maximum entirely.
	if enum := enumList(schema["enum"]); len(enum) > 0 {
		matched := false
		for _, allowed := range enum {
			if enumEqual(val, allowed) {
				matched = true
				break
			}
		}
		if !matched {
			return invalid("parameter %q must be one of %v, got %v", name, enum, val)
		}
	}
	// Numeric bounds (minimum/maximum): only numbers are checked;
	// a bound on a non-number is a schema error, not a call error.
	// Bounds arrive as float64 from JSON or as int/float64 from a
	// Go-built definition; either form is read.
	if f, ok := val.(float64); ok {
		if min, ok := numericBound(schema["minimum"]); ok && f < min {
			return invalid("parameter %q must be >= %v, got %v", name, schema["minimum"], val)
		}
		if max, ok := numericBound(schema["maximum"]); ok && f > max {
			return invalid("parameter %q must be <= %v, got %v", name, schema["maximum"], val)
		}
	}
	// String bounds (minLength/maxLength/pattern): lengths count
	// Unicode code points, as JSON Schema does. A malformed
	// pattern is a schema error, never a call error: it is
	// ignored rather than failing the call.
	if s, ok := val.(string); ok {
		n := len([]rune(s))
		if min, ok := intBound(schema["minLength"]); ok && n < min {
			return invalid("parameter %q must be at least %d characters, got %d", name, min, n)
		}
		if max, ok := intBound(schema["maxLength"]); ok && n > max {
			return invalid("parameter %q must be at most %d characters, got %d", name, max, n)
		}
		if pat, ok := schema["pattern"].(string); ok && pat != "" {
			if re, err := regexp.Compile(pat); err == nil && !re.MatchString(s) {
				return invalid("parameter %q must match pattern %q, got %q", name, pat, s)
			}
		}
	}
	// Nested object schemas: recurse into properties/required.
	if obj, ok := val.(map[string]any); ok {
		if err := validateObject(tool, name, obj, schema); err != nil {
			return err
		}
	}
	// Nested array schemas: each element against items.
	if arr, ok := val.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, elem := range arr {
				if err := validateValue(tool, fmt.Sprintf("%s[%d]", name, i), elem, items); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// enumList reads a schema's "enum" value. Tools publish enums as
// []any (JSON-decoded definitions) or []string / []int / []float64
// (Go-built definitions); the old code accepted only []any, so a
// Go-built []string enum silently imposed no constraint at all
// (Audit 5 F-A5-P2-27). Anything else yields no enum rather than an
// error: a malformed schema must not fail calls.
func enumList(v any) []any {
	switch e := v.(type) {
	case []any:
		return e
	case []string:
		out := make([]any, len(e))
		for i, s := range e {
			out[i] = s
		}
		return out
	case []int:
		out := make([]any, len(e))
		for i, n := range e {
			out[i] = n
		}
		return out
	case []float64:
		out := make([]any, len(e))
		for i, n := range e {
			out[i] = n
		}
		return out
	default:
		return nil
	}
}

// enumEqual reports whether a decoded argument value equals an enum
// entry. Plain DeepEqual is wrong for numbers: JSON decodes argument
// values as float64, while a Go-built enum may hold int, so
// DeepEqual(float64(1), int(1)) is false and a valid value would be
// rejected. Numbers are therefore compared by value; everything
// else uses DeepEqual.
func enumEqual(val, allowed any) bool {
	if vf, ok := toFloat(val); ok {
		if af, ok := toFloat(allowed); ok {
			return vf == af
		}
		return false
	}
	return reflect.DeepEqual(val, allowed)
}

// toFloat converts a numeric value of any Go numeric kind to
// float64. JSON-decoded numbers are float64; Go-built schemas use
// int, int64, or float64. Non-numbers report false.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// numericBound reads a minimum/maximum schema value as a float64.
// A missing or non-numeric bound imposes no constraint.
func numericBound(v any) (float64, bool) {
	return toFloat(v)
}

// intBound reads a minLength/maxLength schema value as an int. A
// JSON-decoded bound is float64; a Go-built one is usually int. A
// missing, non-numeric, or non-integral bound imposes no constraint.
func intBound(v any) (int, bool) {
	f, ok := toFloat(v)
	if !ok || math.Trunc(f) != f {
		return 0, false
	}
	return int(f), true
}

// valueMatchesType reports whether the decoded JSON value v has the
// JSON Schema type t. Numbers decode as float64: "integer" requires
// an integral value, "number" accepts any number. Unknown type names
// never match — the caller treats an unknown declared type as a
// schema the validator cannot apply, and validateValue only calls
// this for known names via schemaTypeNames filtering.
func valueMatchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
	case "null":
		return v == nil
	default:
		return false
	}
}

// jsonTypeName names the JSON type of a decoded value, for error
// messages: string, number, integer, boolean, object, array, null.
func jsonTypeName(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case float64:
		if math.Trunc(v) == v && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// schemaTypeNames extracts the declared type(s) from a schema's
// "type" value: a string, or a list of strings (JSON Schema union).
// Unknown or non-string entries are dropped; an empty result means
// "no usable type constraint".
func schemaTypeNames(v any) []string {
	switch t := v.(type) {
	case string:
		if isKnownJSONType(t) {
			return []string{t}
		}
		return nil
	case []string:
		out := []string{}
		for _, s := range t {
			if isKnownJSONType(s) {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := []string{}
		for _, e := range t {
			if s, ok := e.(string); ok && isKnownJSONType(s) {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// isKnownJSONType reports whether t is a JSON Schema type the
// validator can check.
func isKnownJSONType(t string) bool {
	switch t {
	case "string", "integer", "number", "boolean", "object", "array", "null":
		return true
	default:
		return false
	}
}

// containsType reports whether types names t.
func containsType(types []string, t string) bool {
	for _, s := range types {
		if s == t {
			return true
		}
	}
	return false
}

// describeTypes renders a type list for an error message.
func describeTypes(types []string) string {
	switch len(types) {
	case 0:
		return "a valid value"
	case 1:
		return types[0]
	default:
		return strings.Join(types, " or ")
	}
}

// toStringSlice reads a schema's "required" value, which tools
// publish as []string but which may arrive as []any when a
// definition is built dynamically. Anything else yields no required
// names rather than an error: a malformed schema must not fail calls.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := []string{}
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

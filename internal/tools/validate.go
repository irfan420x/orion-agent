// Package tools — central schema validation for tool arguments.
//
// Registry.Execute validates every call against the tool's own
// Definition before the tool runs: required parameters must be
// present, and every supplied parameter must match its declared JSON
// type (string, integer, number, boolean, object, array) and, when
// the schema declares one, its enum. A validation failure is a clean
// invalid_args ToolError — never a panic, never a half-executed tool —
// and validation runs inside Execute, after the agent's permission
// gate has already decided the call (the gate runs in core before
// Execute is reached) and before any tool code, denylist check, or
// side effect. A call that fails validation therefore cannot bypass
// the gate or the denylist: it never reaches the tool at all.
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
	props, _ := def.Parameters["properties"].(map[string]any)
	required := toStringSlice(def.Parameters["required"])

	for _, name := range required {
		if name == "" {
			continue
		}
		if _, ok := obj[name]; !ok {
			return invalid("missing required parameter %q", name)
		}
	}
	requiredSet := make(map[string]bool, len(required))
	for _, name := range required {
		requiredSet[name] = true
	}
	for name, val := range obj {
		if val == nil && !requiredSet[name] {
			// JSON null for an OPTIONAL parameter means
			// "absent": providers send null for fields they
			// do not fill, and rejecting that would break
			// otherwise-valid calls. A REQUIRED parameter
			// that is present-but-null still fails its type
			// check below (validateValue).
			continue
		}
		prop, ok := props[name]
		if !ok {
			// Unknown parameters are ignored: providers add
			// extra fields, and rejecting them would break
			// calls that are otherwise valid.
			continue
		}
		schema, ok := prop.(map[string]any)
		if !ok {
			continue // malformed property schema: never fail the call
		}
		if err := validateValue(tool, name, val, schema); err != nil {
			return err
		}
	}
	return nil
}

// validateValue checks one supplied parameter against its property
// schema (type and enum). A JSON null is treated as "absent" for an
// optional parameter — providers send null for fields they do not
// fill — but a required parameter that is present-but-null still
// fails its type check unless the schema allows "null".
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
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		for _, allowed := range enum {
			if reflect.DeepEqual(val, allowed) {
				return nil
			}
		}
		return invalid("parameter %q must be one of %v, got %v", name, enum, val)
	}
	return nil
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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// validatingStubTool is a tool with a rich schema, for exercising
// central validation: two required parameters (a string and an
// integer) plus one optional parameter of each JSON type and an
// enum. Execute records whether it ran, so tests can prove a
// rejected call never reached tool code.
type validatingStubTool struct {
	ran *int
}

func (t *validatingStubTool) Name() string { return "stub" }

func (t *validatingStubTool) Description() string { return "Schema-validation stub." }

func (t *validatingStubTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string"},
				"count": map[string]any{"type": "integer"},
				"ratio": map[string]any{"type": "number"},
				"flag":  map[string]any{"type": "boolean"},
				"opts":  map[string]any{"type": "object"},
				"tags":  map[string]any{"type": "array"},
				"mode":  map[string]any{"type": "string", "enum": []any{"a", "b"}},
			},
			"required": []string{"name", "count"},
		},
	}
}

func (t *validatingStubTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	*t.ran++
	return "ran", nil
}

func newValidatingRegistry(t *testing.T) (*Registry, *int) {
	t.Helper()
	r := NewRegistry()
	ran := 0
	if err := r.Register(&validatingStubTool{ran: &ran}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r, &ran
}

// assertInvalidArgs fails unless err is a *ToolError with Kind
// "invalid_args" whose message contains want.
func assertInvalidArgs(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected invalid_args error containing %q, got nil", want)
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *ToolError", err)
	}
	if te.Kind != "invalid_args" {
		t.Fatalf("kind = %q, want invalid_args (err: %v)", te.Kind, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want it to contain %q", err.Error(), want)
	}
}

func TestValidateMissingRequired(t *testing.T) {
	r, ran := newValidatingRegistry(t)
	ctx := context.Background()

	_, err := r.Execute(ctx, "stub", json.RawMessage(`{}`))
	assertInvalidArgs(t, err, `missing required parameter "name"`)
	if *ran != 0 {
		t.Fatalf("tool ran %d time(s) despite missing required parameter", *ran)
	}

	_, err = r.Execute(ctx, "stub", json.RawMessage(`{"name":"x"}`))
	assertInvalidArgs(t, err, `missing required parameter "count"`)
	if *ran != 0 {
		t.Fatalf("tool ran %d time(s) despite missing required parameter", *ran)
	}

	// Empty arguments mean "no arguments": still missing required.
	_, err = r.Execute(ctx, "stub", json.RawMessage(``))
	assertInvalidArgs(t, err, `missing required parameter "name"`)
	if *ran != 0 {
		t.Fatalf("tool ran %d time(s) despite empty arguments", *ran)
	}
}

func TestValidateWrongTypes(t *testing.T) {
	r, ran := newValidatingRegistry(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args string
		want string
	}{
		{"string given number", `{"name":5,"count":2}`, `parameter "name" must be string, got integer`},
		{"integer given fraction", `{"name":"x","count":2.5}`, `parameter "count" must be integer, got number`},
		{"integer given string", `{"name":"x","count":"2"}`, `parameter "count" must be integer, got string`},
		{"number given string", `{"name":"x","count":2,"ratio":"hi"}`, `parameter "ratio" must be number, got string`},
		{"boolean given string", `{"name":"x","count":2,"flag":"yes"}`, `parameter "flag" must be boolean, got string`},
		{"object given array", `{"name":"x","count":2,"opts":[]}`, `parameter "opts" must be object, got array`},
		{"array given object", `{"name":"x","count":2,"tags":{}}`, `parameter "tags" must be array, got object`},
		{"required present but null", `{"name":null,"count":2}`, `parameter "name" must be string, got null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Execute(ctx, "stub", json.RawMessage(tc.args))
			assertInvalidArgs(t, err, tc.want)
			if *ran != 0 {
				t.Fatalf("tool ran despite invalid args %s", tc.args)
			}
		})
	}
}

func TestValidateAcceptsValidCalls(t *testing.T) {
	r, ran := newValidatingRegistry(t)
	ctx := context.Background()

	valid := []string{
		`{"name":"x","count":2}`,
		`{"name":"x","count":2.0}`, // integral float is an integer
		`{"name":"x","count":2,"ratio":1.5}`,
		`{"name":"x","count":2,"ratio":3}`, // integer is a number
		`{"name":"x","count":2,"flag":true}`,
		`{"name":"x","count":2,"opts":{"k":1}}`,
		`{"name":"x","count":2,"tags":["a"]}`,
		`{"name":"x","count":2,"mode":"a"}`,
		`{"name":"x","count":2,"ratio":null}`, // null optional = absent
		`{"name":"x","count":2,"extra":123}`,  // unknown fields ignored
	}
	for _, args := range valid {
		if _, err := r.Execute(ctx, "stub", json.RawMessage(args)); err != nil {
			t.Fatalf("Execute(%s): expected success, got %v", args, err)
		}
	}
	if *ran != len(valid) {
		t.Fatalf("tool ran %d time(s), want %d", *ran, len(valid))
	}
}

func TestValidateEnum(t *testing.T) {
	r, ran := newValidatingRegistry(t)
	ctx := context.Background()

	_, err := r.Execute(ctx, "stub", json.RawMessage(`{"name":"x","count":2,"mode":"c"}`))
	assertInvalidArgs(t, err, `parameter "mode" must be one of`)
	if *ran != 0 {
		t.Fatalf("tool ran despite enum violation")
	}
}

func TestValidateNonObjectArgs(t *testing.T) {
	r, ran := newValidatingRegistry(t)
	ctx := context.Background()

	for _, args := range []string{`[1,2]`, `"hi"`, `42`, `not json`, `{"name":"x"} trailing`} {
		_, err := r.Execute(ctx, "stub", json.RawMessage(args))
		assertInvalidArgs(t, err, "arguments must be a JSON object")
		if *ran != 0 {
			t.Fatalf("tool ran despite non-object args %q", args)
		}
	}
}

func TestValidateNoSchemaMeansNoConstraint(t *testing.T) {
	r := NewRegistry()
	tool := &namedStubTool{name: "schemaless"}
	// namedStubTool publishes empty properties and no required
	// fields; validation must not invent constraints for it.
	if err := r.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := r.Execute(context.Background(), "schemaless", json.RawMessage(`{"anything":true}`)); err != nil {
		t.Fatalf("Execute(schemaless): expected success, got %v", err)
	}
}

// schemaStubTool is a tool with an arbitrary caller-supplied schema,
// for exercising validation of schema shapes the built-in stub does
// not cover (Go-built enums, nested schemas, bounds).
type schemaStubTool struct {
	name   string
	params map[string]any
	ran    *int
}

func (t *schemaStubTool) Name() string { return t.name }

func (t *schemaStubTool) Description() string { return "Schema stub." }

func (t *schemaStubTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.name,
		Description: t.Description(),
		Parameters:  t.params,
	}
}

func (t *schemaStubTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	*t.ran++
	return "ran", nil
}

func newSchemaStubRegistry(t *testing.T, name string, params map[string]any) (*Registry, *int) {
	t.Helper()
	r := NewRegistry()
	ran := 0
	if err := r.Register(&schemaStubTool{name: name, params: params, ran: &ran}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r, &ran
}

func TestValidateEnumStringSlice(t *testing.T) {
	// Audit 5 F-A5-P2-27: the old code checked the enum only if it
	// was []any, so a Go-built definition using []string silently
	// imposed no enum at all.
	r, ran := newSchemaStubRegistry(t, "stub", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []string{"a", "b"}},
		},
	})
	ctx := context.Background()

	_, err := r.Execute(ctx, "stub", json.RawMessage(`{"mode":"c"}`))
	assertInvalidArgs(t, err, `parameter "mode" must be one of`)
	if *ran != 0 {
		t.Fatalf("tool ran despite []string enum violation")
	}
	if _, err := r.Execute(ctx, "stub", json.RawMessage(`{"mode":"a"}`)); err != nil {
		t.Fatalf("Execute(mode=a): expected success, got %v", err)
	}
	if *ran != 1 {
		t.Fatalf("tool ran %d time(s), want 1", *ran)
	}
}

func TestValidateEnumNumericNormalisation(t *testing.T) {
	// reflect.DeepEqual(float64, int) is false, so a JSON-decoded
	// value 1 (float64) never matched a Go-built int enum entry.
	// Numbers must be compared by value.
	r, ran := newSchemaStubRegistry(t, "stub", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level": map[string]any{"type": "integer", "enum": []any{1, 2}},
			"score": map[string]any{"type": "integer", "enum": []int{10, 20}},
			"ratio": map[string]any{"type": "number", "enum": []float64{0.5, 1.5}},
		},
	})
	ctx := context.Background()

	for _, args := range []string{
		`{"level":1}`,
		`{"score":20}`,
		`{"ratio":1.5}`,
	} {
		if _, err := r.Execute(ctx, "stub", json.RawMessage(args)); err != nil {
			t.Fatalf("Execute(%s): expected success, got %v", args, err)
		}
	}
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"level":3}`, `parameter "level" must be one of`},
		{`{"score":15}`, `parameter "score" must be one of`},
		{`{"ratio":2.5}`, `parameter "ratio" must be one of`},
	} {
		_, err := r.Execute(ctx, "stub", json.RawMessage(tc.args))
		assertInvalidArgs(t, err, tc.want)
	}
	if *ran != 3 {
		t.Fatalf("tool ran %d time(s), want 3 (only the valid calls)", *ran)
	}
}

func TestValidateNestedSchemas(t *testing.T) {
	// Audit 5 F-A5-P2-27: validation used to check only top-level
	// properties/required, so nested required fields (e.g.
	// todo_write's todos[].content/status) were ignored entirely.
	// The schema below mirrors todo_write's todos shape.
	r, ran := newSchemaStubRegistry(t, "stub", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "minLength": 1},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "done"}},
					},
					"required": []string{"content", "status"},
				},
			},
		},
	})
	ctx := context.Background()

	cases := []struct {
		name string
		args string
		want string
	}{
		{"both required fields missing", `{"todos":[{"wrong":1},{}]}`, `missing required parameter "todos[0].content"`},
		{"status missing", `{"todos":[{"content":"x"}]}`, `missing required parameter "todos[0].status"`},
		{"second item missing content", `{"todos":[{"content":"x","status":"done"},{}]}`, `missing required parameter "todos[1].content"`},
		{"nested enum violated", `{"todos":[{"content":"x","status":"bogus"}]}`, `parameter "todos[0].status" must be one of`},
		{"nested minLength violated", `{"todos":[{"content":"","status":"done"}]}`, `parameter "todos[0].content" must be at least 1 characters`},
		{"nested wrong type", `{"todos":[{"content":7,"status":"done"}]}`, `parameter "todos[0].content" must be string, got integer`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Execute(ctx, "stub", json.RawMessage(tc.args))
			assertInvalidArgs(t, err, tc.want)
		})
	}
	if *ran != 0 {
		t.Fatalf("tool ran %d time(s) despite nested violations", *ran)
	}

	// A fully valid nested call runs.
	if _, err := r.Execute(ctx, "stub", json.RawMessage(`{"todos":[{"content":"x","status":"done"},{"content":"y","status":"pending"}]}`)); err != nil {
		t.Fatalf("Execute(valid nested): expected success, got %v", err)
	}
	if *ran != 1 {
		t.Fatalf("tool ran %d time(s), want 1", *ran)
	}
}

func TestValidateNumericAndStringBounds(t *testing.T) {
	// Audit 5 F-A5-P2-27: minimum/maximum/pattern/minLength were
	// never read, though schemas declare them (e.g. shell timeout
	// 1–120). Bounds use Go ints here, as Go-built definitions do.
	r, ran := newSchemaStubRegistry(t, "stub", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timeout": map[string]any{"type": "number", "minimum": 1, "maximum": 120},
			"code":    map[string]any{"type": "string", "pattern": "^[a-z]+$", "minLength": 3, "maxLength": 5},
		},
	})
	ctx := context.Background()

	cases := []struct {
		name string
		args string
		want string
	}{
		{"below minimum", `{"timeout":0}`, `parameter "timeout" must be >= 1`},
		{"above maximum", `{"timeout":121}`, `parameter "timeout" must be <= 120`},
		{"too short", `{"code":"ab"}`, `parameter "code" must be at least 3 characters`},
		{"too long", `{"code":"abcdef"}`, `parameter "code" must be at most 5 characters`},
		{"pattern mismatch", `{"code":"ABC"}`, `parameter "code" must match pattern`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Execute(ctx, "stub", json.RawMessage(tc.args))
			assertInvalidArgs(t, err, tc.want)
		})
	}
	if *ran != 0 {
		t.Fatalf("tool ran %d time(s) despite bound violations", *ran)
	}

	for _, args := range []string{`{"timeout":60}`, `{"timeout":1}`, `{"timeout":120}`, `{"code":"abc"}`} {
		if _, err := r.Execute(ctx, "stub", json.RawMessage(args)); err != nil {
			t.Fatalf("Execute(%s): expected success, got %v", args, err)
		}
	}
	if *ran != 4 {
		t.Fatalf("tool ran %d time(s), want 4", *ran)
	}
}

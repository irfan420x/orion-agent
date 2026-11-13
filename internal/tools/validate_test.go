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

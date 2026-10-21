package expr_test

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/flow/expr"
)

func resolver() expr.Resolver {
	types := map[string]expr.Type{
		"trigger.subject.project":      expr.String,
		"trigger.subject.key":          expr.String,
		"trigger.subject":              expr.Object,
		"trigger.comment.text":         expr.String,
		"trigger.payload":              expr.Any,
		"params.projects":              expr.Array,
		"params.dry_run":               expr.Bool,
		"run.id":                       expr.String,
		"steps.evaluate.output.action": expr.String,
		"tasks.primary.state":          expr.String,
		"tasks.primary.refusals":       expr.Array,
	}
	return expr.ResolverFunc(func(path []string) (expr.Type, bool) {
		name := strings.Join(path, ".")
		if name == "trigger.payload" || strings.HasPrefix(name, "trigger.payload.") {
			return expr.Any, true
		}
		typ, ok := types[name]
		return typ, ok
	})
}

func context() map[string]any {
	return map[string]any{
		"trigger": map[string]any{
			"subject": map[string]any{"project": "TEST", "key": "TEST-12"},
			"comment": map[string]any{"text": "/opencode start"},
			"payload": map[string]any{"project": map[string]any{"shortName": "TEST"}},
		},
		"params": map[string]any{
			"projects": []any{"TEST", "OTHER"},
			"dry_run":  true,
		},
		"run": map[string]any{"id": "run_1"},
		"steps": map[string]any{
			"evaluate": map[string]any{"output": map[string]any{"action": "execute"}},
		},
		"tasks": map[string]any{
			"primary": map[string]any{
				"state":    "awaiting_input",
				"refusals": []any{"git push", "rm -rf"},
			},
		},
	}
}

func TestExpressions(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   any
	}{
		{name: "string equality", source: `trigger.subject.project == 'TEST'`, want: true},
		{name: "key", source: `trigger.subject.key`, want: "TEST-12"},
		{name: "not equal", source: `trigger.subject.project != 'OTHER'`, want: true},
		{name: "and", source: `trigger.subject.project == 'TEST' && params.dry_run`, want: true},
		{name: "or", source: `false || params.dry_run`, want: true},
		{name: "not", source: `!false`, want: true},
		{name: "in", source: `trigger.subject.project in params.projects`, want: true},
		{name: "matches", source: `trigger.comment.text matches '(?i)^\\s*/opencode start\\b'`, want: true},
		{name: "len string", source: `len(trigger.subject.key)`, want: 7},
		{name: "len array", source: `len(tasks.primary.refusals)`, want: 2},
		{name: "has", source: `has(trigger.payload, 'project')`, want: true},
		{name: "has missing", source: `has(trigger.payload, 'issue')`, want: false},
		{name: "greater", source: `len(tasks.primary.refusals) > 1`, want: true},
		{name: "dynamic payload", source: `trigger.payload.project.shortName`, want: "TEST"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program, err := expr.Compile(test.source, resolver())
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := program.Eval(context())
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != test.want {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCompileRejects(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "unknown path", source: `trigger.subject.missing`, want: "unknown path"},
		{name: "unknown root", source: `nope.value`, want: "unknown path"},
		{name: "and needs bool", source: `trigger.subject.key && true`, want: "needs bools"},
		{name: "not needs bool", source: `!trigger.subject.key`, want: "needs a bool"},
		{name: "ordered needs compatible", source: `trigger.subject.key < 3`, want: "two numbers or two strings"},
		{name: "in needs array", source: `trigger.subject.project in trigger.subject.key`, want: "array on the right"},
		{name: "matches needs strings", source: `trigger.subject.key matches 3`, want: "matches needs strings"},
		{name: "bad regex", source: `trigger.comment.text matches '['`, want: "error parsing regexp"},
		{name: "unknown function", source: `upper(trigger.subject.key)`, want: "unknown function"},
		{name: "len arity", source: `len()`, want: "len takes one argument"},
		{name: "has needs object", source: `has(trigger.subject.key, 'x')`, want: "has needs an object"},
		{name: "unclosed paren", source: `(true`, want: "expected )"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := expr.Compile(test.source, resolver())
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func TestEvalRejectsUnknownPath(t *testing.T) {
	program, err := expr.Compile(`trigger.payload.issue.fields`, resolver())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := program.Eval(map[string]any{}); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}

func TestTemplate(t *testing.T) {
	template, err := expr.CompileTemplate(
		"Analyze {{ trigger.subject.key }} in {{ trigger.subject.project }}", resolver())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := template.Render(context())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Analyze TEST-12 in TEST"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTemplateRejectsObject(t *testing.T) {
	_, err := expr.CompileTemplate("{{ trigger.subject }}", resolver())
	if err == nil || !strings.Contains(err.Error(), "renders") {
		t.Fatalf("expected an object render error, got %v", err)
	}
}

func TestTemplateRuntimeNull(t *testing.T) {
	ctx := context()
	ctx["trigger"].(map[string]any)["comment"] = map[string]any{"text": nil}
	template, err := expr.CompileTemplate("{{ trigger.comment.text }}", resolver())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := template.Render(ctx); err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("expected a null render error, got %v", err)
	}
}

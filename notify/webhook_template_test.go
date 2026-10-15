package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file pins the **capability boundary** of generic Webhook templates.
//
// This is the only place in the entire package where "user-provided strings are evaluated as code", so it's important to
// be explicit about what it can and cannot do, and pin those properties with tests — otherwise someone might later add a
// method to the template context or a readFile to the FuncMap, silently expanding the capability surface while the diff
// looks like just a harmless small function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can both access fields and call methods). So as long as the template
// context can reach **any** type with exported methods, those methods are exposed to the template author. This feature's
// context is deliberately all pure data (only exported fields, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem. Before deciding to allow it, first think
// clearly about whether that method could be used by a template to read things that shouldn't be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which opens those methods' capabilities to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal pins the set of functions exposed to templates.
//
// Every additional function in FuncMap adds a capability. Currently only json / jsons exist, which serialize values to
// JSON fragments — cannot read files, cannot make requests, cannot execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, want %v. Before adding a function, confirm it does not "+
			"expand the capability surface (no file reads/writes, no network requests, no command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in templates: accessing something that doesn't exist must
// fail, not echo something; and the failure message must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a nonexistent field should error")
	}
	// Error must not contain real content from template context (vuln title/summary).
	for _, leak := range []string{"SQL injection", "param id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a wrong template is a config error, retry won't self-heal. If judged retryable, one
// bad template would make every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("template syntax errors should be caught at save time")
	}
	// Even bypassing validation and delivering directly, it must be judged permanent failure, not retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a broken template should be a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint "template render result must be valid JSON". It also incidentally
// blocks "use template to generate plain text to trigger other protocols" style abuse.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// Valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering non-JSON must be rejected (not sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a rendered non-JSON body should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error message should identify the JSON problem, got %v", err)
	}
}

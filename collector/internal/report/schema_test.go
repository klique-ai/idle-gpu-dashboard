package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests keep docs/report.schema.json and docs/examples/ in step with
// the Go types. If one fails after you changed a type in report.go, follow
// "Changing the report" in CLAUDE.md: update the schema, docs/payload.md and
// the examples.

const docsDir = "../../../docs"

type jsonSchema map[string]any

func loadSchema(t *testing.T) jsonSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("docs/report.schema.json is not valid JSON: %v", err)
	}
	return s
}

// resolve follows a "$ref" and unwraps a nullable "oneOf: [X, {type: null}]".
func (root jsonSchema) resolve(node map[string]any) (resolved map[string]any, nullable bool) {
	for {
		if ref, ok := node["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/$defs/")
			node = root["$defs"].(map[string]any)[name].(map[string]any)
			continue
		}
		if alts, ok := node["oneOf"].([]any); ok {
			var other map[string]any
			for _, a := range alts {
				m := a.(map[string]any)
				if m["type"] == "null" {
					nullable = true
				} else {
					other = m
				}
			}
			node = other
			continue
		}
		return node, nullable
	}
}

var timeType = reflect.TypeOf(time.Time{})

// schemaType is the JSON Schema "type" a Go type encodes to.
func schemaType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType, t.Kind() == reflect.String:
		return "string"
	case t.Kind() == reflect.Bool:
		return "boolean"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		return "integer"
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		return "number"
	case t.Kind() == reflect.Slice:
		return "array"
	}
	return "object"
}

// compare checks a Go struct type against a schema object, recursively.
func (root jsonSchema) compare(t *testing.T, path string, typ reflect.Type, node map[string]any) {
	props, _ := node["properties"].(map[string]any)
	required := map[string]bool{}
	for _, r := range node["required"].([]any) {
		required[r.(string)] = true
	}
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		seen[name] = true
		where := path + "." + name
		raw, ok := props[name]
		if !ok {
			t.Errorf("%s: field in report.go but missing from report.schema.json", where)
			continue
		}
		omitempty := strings.Contains(opts, "omitempty")
		if required[name] == omitempty {
			t.Errorf("%s: schema says required=%v, but the Go field has omitempty=%v", where, required[name], omitempty)
		}
		child, nullable := root.resolve(raw.(map[string]any))
		if wantNull := f.Type.Kind() == reflect.Pointer && !omitempty; wantNull != nullable {
			t.Errorf("%s: schema nullable=%v, but the Go field can be null=%v", where, nullable, wantNull)
		}
		if _, isConst := child["const"]; !isConst {
			if got, want := child["type"], schemaType(f.Type); got != want {
				t.Errorf("%s: schema type %v, Go type encodes as %q", where, got, want)
			}
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct && ft != timeType:
			root.compare(t, where, ft, child)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			items, _ := root.resolve(child["items"].(map[string]any))
			root.compare(t, where+"[]", ft.Elem(), items)
		}
	}
	var stale []string
	for name := range props {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("%s.%s: in report.schema.json but not in report.go", path, name)
	}
}

func TestSchemaMatchesGoTypes(t *testing.T) {
	root := loadSchema(t)
	props := root["properties"].(map[string]any)
	if got := props["schema"].(map[string]any)["const"]; got != Schema {
		t.Errorf("schema const %q != report.Schema %q", got, Schema)
	}
	root.compare(t, "report", reflect.TypeOf(Report{}), root)
}

// Every example must decode into the current types without unknown fields.
func TestExamplesMatchGoTypes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(docsDir, "examples", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found in docs/examples (%v)", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r Report
		if err := dec.Decode(&r); err != nil {
			t.Errorf("%s: %v (regenerate it: see docs/examples/README.md)", filepath.Base(f), err)
			continue
		}
		if r.Schema != Schema {
			t.Errorf("%s: schema %q, current is %q", filepath.Base(f), r.Schema, Schema)
		}
	}
}

package logging

// A test-local checker for logging-schema.json. It supports exactly the
// keywords the schema uses and fails on any other, so a schema change that
// adds a keyword cannot pass unchecked.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"testing"
	"unicode/utf8"
)

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/logging-schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return s
}

// validate returns every violation of schema by v.
func validate(schema map[string]any, v any, path string) []string {
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, path+": "+fmt.Sprintf(f, a...)) }
	for kw, arg := range schema {
		switch kw {
		case "$schema", "$id", "title", "description":
		case "type":
			if !typeMatches(arg.(string), v) {
				fail("want type %v, got %T", arg, v)
			}
		case "enum":
			ok := false
			for _, e := range arg.([]any) {
				if e == v {
					ok = true
				}
			}
			if !ok {
				fail("%v not in enum %v", v, arg)
			}
		case "pattern":
			if s, ok := v.(string); ok && !regexp.MustCompile(arg.(string)).MatchString(s) {
				fail("%q does not match %s", s, arg)
			}
		case "maxLength":
			if s, ok := v.(string); ok && float64(utf8.RuneCountInString(s)) > arg.(float64) {
				fail("longer than %v", arg)
			}
		case "minLength":
			if s, ok := v.(string); ok && float64(utf8.RuneCountInString(s)) < arg.(float64) {
				fail("shorter than %v", arg)
			}
		case "minimum":
			if n, ok := v.(float64); ok && n < arg.(float64) {
				fail("%v below minimum %v", n, arg)
			}
		case "required":
			if obj, ok := v.(map[string]any); ok {
				for _, k := range arg.([]any) {
					if _, present := obj[k.(string)]; !present {
						fail("missing required key %q", k)
					}
				}
			}
		case "properties":
			if obj, ok := v.(map[string]any); ok {
				for k, sub := range arg.(map[string]any) {
					if val, present := obj[k]; present {
						errs = append(errs, validate(sub.(map[string]any), val, path+"."+k)...)
					}
				}
			}
		case "additionalProperties":
			if b, ok := arg.(bool); !ok || !b {
				fail("additionalProperties other than true is not supported by this checker")
			}
		default:
			fail("checker does not support schema keyword %q", kw)
		}
	}
	return errs
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == math.Trunc(n)
	}
	return false
}

func checkLine(t *testing.T, schema map[string]any, line string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Errorf("not JSON: %q: %v", line, err)
		return
	}
	for _, e := range validate(schema, v, "$") {
		t.Errorf("schema violation in %s\n  %s", line, e)
	}
}

func TestSchemaChecker_RejectsBadLines(t *testing.T) {
	schema := loadSchema(t)
	good := `{"ts":"2026-01-02T03:04:05.678Z","level":"info","msg":"m","service":"s","op":"a.b","status":"ok","duration_ms":0,"error":"","trace_id":""}`
	var v any
	_ = json.Unmarshal([]byte(good), &v)
	if errs := validate(schema, v, "$"); len(errs) != 0 {
		t.Fatalf("good line rejected: %v", errs)
	}
	for name, mut := range map[string]func(map[string]any){
		"missing key":  func(m map[string]any) { delete(m, "trace_id") },
		"bad status":   func(m map[string]any) { m["status"] = "weird" },
		"bad op":       func(m map[string]any) { m["op"] = "Bad Op" },
		"negative dur": func(m map[string]any) { m["duration_ms"] = float64(-1) },
		"bad trace":    func(m map[string]any) { m["trace_id"] = "x" },
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(good), &m)
		mut(m)
		if errs := validate(schema, m, "$"); len(errs) == 0 {
			t.Errorf("%s: checker accepted a bad line", name)
		}
	}
	if errs := validate(map[string]any{"oneOf": []any{}}, v, "$"); len(errs) == 0 {
		t.Error("checker must fail on an unsupported keyword")
	}
}

// keyOrder returns the top-level keys of a JSON object in written order.
func keyOrder(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(line)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %q", line)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

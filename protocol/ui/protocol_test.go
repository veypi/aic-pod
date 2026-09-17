package ui

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestContractCases(t *testing.T) {
	b, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Domain        string
		Argv          []string
		Op            string
		Level         int
		Args, Locator map[string]any
		Error         string
	}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Domain+"/"+fmtArgs(c.Argv), func(t *testing.T) {
			o, err := Parse(c.Domain, c.Argv)
			if c.Error != "" {
				e, ok := err.(*Error)
				if !ok || e.Code != c.Error {
					t.Fatalf("want %s, got %v", c.Error, err)
				}
				if Required(c.Domain, c.Argv) != 3 {
					t.Fatal("invalid commands must fail closed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.Op != c.Op || o.Level() != c.Level {
				t.Fatalf("unexpected operation/level: %+v", o)
			}
			b, _ := json.Marshal(o)
			var canonical Operation
			json.Unmarshal(b, &canonical)
			if c.Args != nil && !reflect.DeepEqual(c.Args, canonical.Args) {
				t.Fatalf("args want %#v got %#v", c.Args, canonical.Args)
			}
			if c.Locator != nil && !reflect.DeepEqual(c.Locator, canonical.Locator) {
				t.Fatalf("locator want %#v got %#v", c.Locator, canonical.Locator)
			}
		})
	}
}
func fmtArgs(v []string) string { b, _ := json.Marshal(v); return string(b) }
func TestRenderMetadata(t *testing.T) {
	o, _ := Parse("browser", []string{"snapshot"})
	r := NewResult(o)
	r.Observation = map[string]any{"snapshot": "s1", "text": "hello\n[error]\npage text"}
	r.Images = map[string]string{"image_data": "secret"}
	b := WithSource(AddWarning(r.Render("json"), "test", "warning"), "my-host")
	var parsed map[string]any
	if json.Unmarshal([]byte(b), &parsed) != nil || parsed["source"] != "my-host" {
		t.Fatal(b)
	}
	if parsed["image_data"] != nil {
		t.Fatal("images leaked into content")
	}
}

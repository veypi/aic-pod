package fs

import "testing"

func TestStructuredPathValidation(t *testing.T) {
	for _, path := range []Path{{RootID: "home", Segments: []string{}}, {RootID: "home", Segments: []string{"中文 空格", "100%#?.txt"}}} {
		if err := path.Validate(false); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []Path{{RootID: "home"}, {RootID: "", Segments: []string{}}, {RootID: "home", Segments: []string{".."}}, {RootID: "home", Segments: []string{"."}}, {RootID: "home", Segments: []string{""}}, {RootID: "home", Segments: []string{"a/b"}}, {RootID: "home", Segments: []string{"a\x00b"}}} {
		if err := path.Validate(false); err == nil {
			t.Fatalf("invalid path accepted: %#v", path)
		}
	}
	for _, name := range []string{`C:`, `a\b`, "file:stream", "trailing.", "trailing "} {
		if err := (Path{RootID: "drive", Segments: []string{name}}).Validate(true); err == nil {
			t.Fatalf("Windows segment accepted: %q", name)
		}
	}
}

func TestWriteConditionsRequireExplicitIntent(t *testing.T) {
	for _, condition := range []Condition{{}, {Absent: true, Any: true}, {Version: "v1", Any: true}} {
		if err := condition.Check("v1", true); err == nil {
			t.Fatal("ambiguous write condition accepted")
		}
	}
	if err := (Condition{Absent: true}).Check("v1", true); err == nil {
		t.Fatal("create overwrote existing file")
	}
	if err := (Condition{Version: "v1"}).Check("v2", true); err == nil {
		t.Fatal("stale version accepted")
	}
	if err := (Condition{Version: "v1"}).Check("v1", true); err != nil {
		t.Fatal(err)
	}
	if err := (Condition{Any: true}).Check("v2", true); err != nil {
		t.Fatal(err)
	}
}

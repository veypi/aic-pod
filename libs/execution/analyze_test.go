package execution

import "testing"

func TestAnalyzeGrantAndSyntax(t *testing.T) {
	for _, script := range []string{`grant fs '/work/a b'`, `echo ok | cat; grant net example.com:443`} {
		a := Analyze(script, nil)
		if a.SyntaxError != "" || !a.HasGrant {
			t.Fatalf("%q: %+v", script, a)
		}
	}
	if a := Analyze(`if then fi`, nil); a.SyntaxError == "" {
		t.Fatal("missing syntax error")
	}
	// Literal grant with dynamic arguments still needs preflight approval.
	if a := Analyze(`grant fs "$TARGET"`, nil); !a.HasGrant {
		t.Fatalf("dynamic arguments lost grant detection: %+v", a)
	}
}

func TestAnalyzeScriptRecursion(t *testing.T) {
	files := map[string][]byte{"inner.sh": []byte("grant fs /work; source loop.sh"), "loop.sh": []byte("source inner.sh")}
	a := Analyze("bash inner.sh", func(p string) ([]byte, error) { return files[p], nil })
	if a.SyntaxError != "" || !a.HasGrant {
		t.Fatalf("recursive grants: %+v", a)
	}
}

func TestAnalyzeDoesNotGuessDynamicCommands(t *testing.T) {
	for _, script := range []string{`echo grant fs /tmp`, `cmd=grant; "$cmd" fs /tmp`} {
		if a := Analyze(script, nil); a.HasGrant {
			t.Fatalf("guessed command: %+v", a)
		}
	}
}

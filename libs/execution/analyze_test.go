package execution

import "testing"

func TestAnalyzeGrantAndSyntax(t *testing.T) {
	for _, script := range []string{`grant fs '/work/a b'`, `echo ok | cat; grant net example.com:443`} {
		a := Analyze(script, nil)
		if a.SyntaxError != "" || len(a.GrantRequests) != 1 {
			t.Fatalf("%q: %+v", script, a)
		}
	}
	if a := Analyze(`if then fi`, nil); a.SyntaxError == "" {
		t.Fatal("missing syntax error")
	}
	// Dynamic grants are checked at execution by the trusted approval context.
	if a := Analyze(`grant fs "$TARGET"`, nil); len(a.GrantRequests) != 0 {
		t.Fatalf("dynamic target guessed: %+v", a)
	}
}

func TestAnalyzeScriptRecursion(t *testing.T) {
	files := map[string][]byte{"inner.sh": []byte("grant fs /work; source loop.sh"), "loop.sh": []byte("source inner.sh")}
	a := Analyze("bash inner.sh", func(p string) ([]byte, error) { return files[p], nil })
	if a.SyntaxError != "" || len(a.GrantRequests) != 1 || a.GrantRequests[0].Target != "/work" {
		t.Fatalf("recursive grants: %+v", a)
	}
}

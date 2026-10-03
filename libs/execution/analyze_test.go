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

func TestAnalyzeGrantReadOnlyCommands(t *testing.T) {
	for _, tc := range []struct {
		script   string
		approval bool
	}{
		{`grant`, false},
		{`grant status`, false},
		{`'grant' "status"`, false},
		{`grant --help`, false},
		{`grant -h`, false},
		{`grant help`, false},
		{`grant status; grant --help`, false},
		{`grant status "$DETAIL"`, false},
		{`grant status | cat`, false},
		{`grant status; grant fs /tmp`, true},
		{`grant --help; grant net example.com:443`, true},
		{`grant status "$(grant fs /tmp)"`, true},
		{`grant --help "$(grant fs /tmp)"`, true},
		{`grant "$ACTION"`, true},
		{`grant fs --help`, true},
		{`grant --permanent fs /tmp`, true},
	} {
		t.Run(tc.script, func(t *testing.T) {
			a := Analyze(tc.script, nil)
			if a.SyntaxError != "" || a.HasGrant != tc.approval {
				t.Fatalf("approval=%v analysis=%+v", tc.approval, a)
			}
		})
	}
	files := map[string]string{"view.sh": "grant status; grant --help", "request.sh": "grant status; grant fs /tmp"}
	for _, name := range []string{"view.sh", "request.sh"} {
		a := Analyze("source "+name, func(path string) ([]byte, error) { return []byte(files[path]), nil })
		if a.SyntaxError != "" || a.HasGrant != (name == "request.sh") {
			t.Fatalf("%s: %+v", name, a)
		}
	}
}

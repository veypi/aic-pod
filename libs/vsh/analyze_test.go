package vsh

import (
	"strings"
	"testing"
)

func TestAnalyzeRedirects(t *testing.T) {
	t.Parallel()
	a := Analyze(`echo x > out.txt; echo y >> log.txt; cat < in.txt > copy.txt`, nil)
	want := map[string]bool{"out.txt": true, "log.txt": true, "copy.txt": true}
	if len(a.WriteTargets) != len(want) {
		t.Fatalf("writes = %v", a.WriteTargets)
	}
	for _, w := range a.WriteTargets {
		if !want[w] {
			t.Fatalf("unexpected write target %q", w)
		}
	}
	if a.UsesNetwork || a.SyntaxError != "" {
		t.Fatalf("%+v", a)
	}
}

// TestAnalyzeVirtualDevicesExcluded /dev 虚拟设备与 std 别名不算写目标（引擎
// 运行时自管，不到达 FS 适配器——2026-09-24 实测：`>/dev/null`、`>/dev/stdout`
// 被静态预检误判越界写而整段拒绝）。
func TestAnalyzeVirtualDevicesExcluded(t *testing.T) {
	t.Parallel()
	a := Analyze(`echo x > /dev/null; echo e 2>/dev/null; cat f > /dev/zero; echo o > /dev/stdout; echo e > /dev/stderr; echo y > out.txt`, nil)
	if len(a.WriteTargets) != 1 || a.WriteTargets[0] != "out.txt" {
		t.Fatalf("writes = %v, want [out.txt]", a.WriteTargets)
	}
	// /dev/random 不在豁免单（写 random 无意义），照常拒。
	a = Analyze(`echo x > /dev/random`, nil)
	if len(a.WriteTargets) != 1 || a.WriteTargets[0] != "/dev/random" {
		t.Fatalf("writes = %v, want [/dev/random]", a.WriteTargets)
	}
}

func TestAnalyzeWriteArgTable(t *testing.T) {
	t.Parallel()
	a := Analyze(`cp a.txt b.txt; mv x y; tee t1 t2 < /dev/null; mkdir -p d1 d2; rm -rf junk; sed -i s/a/b/ f.txt; curl -o dl.bin https://x/y`, nil)
	joined := strings.Join(a.WriteTargets, ",")
	for _, want := range []string{"b.txt", "y", "t1", "t2", "d1", "d2", "junk", "f.txt", "dl.bin"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, a.WriteTargets)
		}
	}
	if !a.UsesNetwork {
		t.Fatal("curl should set UsesNetwork")
	}
}

func TestAnalyzeDynamicSkipped(t *testing.T) {
	t.Parallel()
	// $X 动态目标不收集（运行期规则表门强制，验收 4）。
	a := Analyze(`rm $X; echo hi > $OUT`, nil)
	if len(a.WriteTargets) != 0 {
		t.Fatalf("dynamic targets should be skipped: %v", a.WriteTargets)
	}
}

// F3 回归（2026-09-24 实测）：末位词动态时，不得把倒数第二个字面词误报为
// 写目标（`ln -s <字面target> $L` 里 target 是读操作）。
func TestAnalyzeLnDynamicLinkName(t *testing.T) {
	t.Parallel()
	a := Analyze(`ln -s /u/admin/skills $L`, nil)
	if len(a.WriteTargets) != 0 {
		t.Fatalf("ln target (read) must not be flagged when link name is dynamic: %v", a.WriteTargets)
	}
	// 对照：末位字面仍取为写目标。
	b := Analyze(`ln -s /tmp/x /tmp/y`, nil)
	if len(b.WriteTargets) != 1 || b.WriteTargets[0] != "/tmp/y" {
		t.Fatalf("literal link name should be flagged: %v", b.WriteTargets)
	}
}

// 逐词判定改进：前面的动态词不截断后面的字面写目标。
func TestAnalyzeDynamicPrefixNotTruncating(t *testing.T) {
	t.Parallel()
	a := Analyze(`cp $SRC dst.txt; touch a.txt $B c.txt`, nil)
	joined := strings.Join(a.WriteTargets, ",")
	for _, want := range []string{"dst.txt", "a.txt", "c.txt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, a.WriteTargets)
		}
	}
	for _, notWant := range []string{"$SRC", "$B"} {
		if strings.Contains(joined, notWant) {
			t.Fatalf("dynamic word leaked into targets: %v", a.WriteTargets)
		}
	}
}

func TestAnalyzeSyntaxError(t *testing.T) {
	t.Parallel()
	a := Analyze(`if then fi <<<`, nil)
	if a.SyntaxError == "" {
		t.Fatal("want syntax error")
	}
}

func TestAnalyzeQuotedLiteral(t *testing.T) {
	t.Parallel()
	a := Analyze(`echo x > "my file.txt"; echo y > 'single.txt'`, nil)
	if len(a.WriteTargets) != 2 || a.WriteTargets[0] != "my file.txt" || a.WriteTargets[1] != "single.txt" {
		t.Fatalf("%v", a.WriteTargets)
	}
}

func TestAnalyzeScriptRecursion(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{
		"inner.sh": []byte("echo deep > deep-out.txt\n"),
		"loop.sh":  []byte("bash loop.sh\n"),
	}
	read := func(path string) ([]byte, error) { return files[path], nil }
	a := Analyze("bash inner.sh", read)
	if len(a.WriteTargets) != 1 || a.WriteTargets[0] != "deep-out.txt" {
		t.Fatalf("recursion = %v", a.WriteTargets)
	}
	// 自递归不死循环（seen 去重）。
	a = Analyze("bash loop.sh", read)
	if a.SyntaxError != "" {
		t.Fatalf("%+v", a)
	}
}

func TestAnalyzeNetworkCommands(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"curl https://a.b", "wget -O f https://a.b"} {
		if a := Analyze(script, nil); !a.UsesNetwork {
			t.Fatalf("%q should use network", script)
		}
	}
	if a := Analyze("jq . f.json", nil); a.UsesNetwork {
		t.Fatal("jq is local")
	}
}

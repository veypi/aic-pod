package host

import (
	"sort"
	"testing"
)

// 版本目录「取最新」按自然序：数字段比数值（Blender 4.10 > 4.2），字典序会选错。
func TestNaturalLessVersions(t *testing.T) {
	got := []string{"Blender 4.10", "Blender 4.2", "Blender 3.6", "Blender 4.10.1", "Blender 4.2.1"}
	sort.Slice(got, func(i, j int) bool { return naturalLess(got[i], got[j]) })
	want := []string{"Blender 3.6", "Blender 4.2", "Blender 4.2.1", "Blender 4.10", "Blender 4.10.1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("natural order: %v (want %v)", got, want)
		}
	}
	// 前导零与等值保确定性（数值相等时少零段排前，双向互斥）
	if !naturalLess("v1.1", "v1.01") || naturalLess("v1.01", "v1.1") {
		t.Fatal("leading-zero tie-break must be deterministic")
	}
	if naturalLess("a", "a") || !naturalLess("a", "ab") {
		t.Fatal("prefix ordering wrong")
	}
}

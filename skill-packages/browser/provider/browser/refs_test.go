package browser

import (
	"context"
	"fmt"
	"testing"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

func TestRefTableEvictsLeastRecentlyUsed(t *testing.T) {
	tab := newRefTable()
	for i := 0; i < maxRefsPerPage; i++ {
		tab.put(fmt.Sprintf("ref_%d", i), reference{backend: i})
	}
	tab.put("ref_extra", reference{})
	if _, ok := tab.get("ref_0"); ok {
		t.Fatal("oldest ref retained past capacity")
	}
	if _, ok := tab.get("ref_1"); !ok {
		t.Fatal("newer ref evicted")
	}
	if _, ok := tab.get("ref_extra"); !ok {
		t.Fatal("fresh insert evicted")
	}
	if tab.ll.Len() != maxRefsPerPage {
		t.Fatal("capacity not bounded:", tab.ll.Len())
	}
}

func TestRefTableTouchOnGet(t *testing.T) {
	tab := newRefTable()
	for i := 0; i < maxRefsPerPage; i++ {
		tab.put(fmt.Sprintf("ref_%d", i), reference{})
	}
	if _, ok := tab.get("ref_0"); !ok {
		t.Fatal("ref missing before overflow")
	}
	tab.put("ref_extra", reference{})
	if _, ok := tab.get("ref_0"); !ok {
		t.Fatal("touched ref evicted")
	}
	if _, ok := tab.get("ref_1"); ok {
		t.Fatal("untouched older ref retained")
	}
}

func TestRefTableHasNoClockExpiry(t *testing.T) {
	// 纯序 LRU 无墙钟维度：未满容的引用永远有效，无需 fake clock。
	tab := newRefTable()
	tab.put("ref_old", reference{backend: 1})
	if r, ok := tab.get("ref_old"); !ok || r.backend != 1 {
		t.Fatal("ref expired without capacity pressure")
	}
}

func TestResolveKeepsStaleDocumentCheck(t *testing.T) {
	p := &page{info: PageInfo{Document: "doc_new"}, refs: newRefTable()}
	p.refs.put("ref_1", reference{backend: 1, document: "doc_old"})
	if _, err := p.resolve(context.Background(), Locator{Ref: "ref_1"}); err == nil || wire.AsFault(err).Code != "stale_ref" {
		t.Fatal("stale document accepted:", err)
	}
	if _, err := p.resolve(context.Background(), Locator{Ref: "ref_missing"}); err == nil || wire.AsFault(err).Code != "stale_ref" {
		t.Fatal("unknown ref accepted:", err)
	}
}

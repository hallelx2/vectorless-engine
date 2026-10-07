package ingest

import (
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// The shape prod built for the 3M 2018 10-K (contents on page 2).
func threeMShape() []tree.TOCNode {
	leaf := func(t string, p int) tree.TOCNode { return tree.TOCNode{Title: t, StartPage: p} }
	return []tree.TOCNode{
		{Title: "PART I", StartPage: 4, Nodes: []tree.TOCNode{
			leaf("ITEM 1", 4), leaf("ITEM 1A", 10), leaf("ITEM 1B", 12), leaf("ITEM 2", 12), leaf("ITEM 3", 12), leaf("ITEM 4", 12),
		}},
		{Title: "PART II", StartPage: 2, Nodes: []tree.TOCNode{
			leaf("ITEM 5", 13), leaf("ITEM 6", 14), leaf("ITEM 7", 15),
			leaf("Performance by Geographic Area", 38),
			{Title: "Critical Accounting Estimates", StartPage: 2, Nodes: []tree.TOCNode{leaf("Legal", 39), leaf("Pension", 40)}},
			leaf("New Accounting Pronouncements", 67),
			leaf("Financial Condition and Liquidity", 43), leaf("Financial Instruments", 50), leaf("ITEM 7A", 51), leaf("ITEM 8", 52),
		}},
	}
}

func TestRepairStartsThenDeriveOn3MShape(t *testing.T) {
	nodes := threeMShape()
	if n := repairStarts(nodes, []int{2}); n != 3 {
		t.Fatalf("cleared %d starts, want 3 (PART II and Critical Accounting Estimates on the contents page, Pronouncements out of order)", n)
	}
	deriveEndPages(nodes, 160)

	part1, part2 := nodes[0], nodes[1]
	if part2.StartPage != 13 {
		t.Errorf("PART II starts on %d, want 13 (its first item)", part2.StartPage)
	}
	if part1.EndPage != 12 {
		t.Errorf("PART I ends on %d, want 12", part1.EndPage)
	}
	for _, it := range part1.Nodes[2:] {
		if it.StartPage != 12 || it.EndPage != 12 {
			t.Errorf("%s = %d-%d, want 12-12 (items sharing a page are one page long)", it.Title, it.StartPage, it.EndPage)
		}
	}
	byTitle := map[string]tree.TOCNode{}
	for _, n := range part2.Nodes {
		byTitle[n.Title] = n
	}
	if c := byTitle["Critical Accounting Estimates"]; c.StartPage != 39 {
		t.Errorf("Critical Accounting Estimates starts on %d, want 39 (its first child)", c.StartPage)
	}
	if n := byTitle["New Accounting Pronouncements"]; n.StartPage != 0 {
		t.Errorf("the out-of-order entry keeps page %d; want it cleared", n.StartPage)
	}
	for _, title := range []string{"Financial Condition and Liquidity", "Financial Instruments", "ITEM 7A", "ITEM 8"} {
		if byTitle[title].StartPage == 0 {
			t.Errorf("%s was cleared; one misplaced sibling must not clear the correct ones after it", title)
		}
	}
}

func TestOutOfOrderKeepsTheLongestRun(t *testing.T) {
	ns := []tree.TOCNode{{StartPage: 38}, {StartPage: 2}, {StartPage: 67}, {StartPage: 43}, {StartPage: 50}, {StartPage: 0}, {StartPage: 52}}
	got := outOfOrder(ns)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("outOfOrder = %v, want [1 2] (pages 2 and 67)", got)
	}
}

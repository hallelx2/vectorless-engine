// Command navbench measures retrieval navigation on a Judge against
// FinanceBench's gold evidence pages, with no generative model.
//
// For each question on a filing with a tree: rank the tree's leaves,
// read the pages of the best few, rank those pages, and ask whether the
// gold evidence pages are in the evidence set.
//
//	go run ./cmd/navbench -questions ~/.cache/vlbench/financebench-questions.jsonl \
//	  -trees ~/.cache/vlbench/trees-jev2 -pdfs ~/.cache/vlbench/financebench -out nav.jsonl
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hallelx2/llmgate/judge/typesafe"
	"github.com/hallelx2/llmgate/middleware/retry"

	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

type question struct {
	ID       string `json:"id"`
	Doc      string `json:"doc"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Evidence []int  `json:"evidence_pages"`
	Type     string `json:"type"`
}

type dump struct {
	Doc   string         `json:"doc"`
	Nodes []tree.TOCNode `json:"nodes"`
}

type outcome struct {
	ID            string    `json:"id"`
	Doc           string    `json:"doc"`
	Question      string    `json:"question"`
	Gold          []int     `json:"gold_pages"`
	Selected      []string  `json:"selected_leaves"`
	SelectedPages [][2]int  `json:"selected_ranges"`
	Evidence      []int     `json:"evidence_pages"`
	EvidenceP     []float64 `json:"evidence_p"`
	PagesRead     int       `json:"pages_read"`
	LeafHit       bool      `json:"leaf_hit"` // every gold page inside a selected leaf
	Recall        float64   `json:"recall"`   // share of gold pages in the evidence set
	Hit           bool      `json:"hit"`      // recall == 1
	Requests      int       `json:"requests"`
	InTokens      int       `json:"in_tokens"`
	CostUSD       float64   `json:"cost_usd"`
	Seconds       float64   `json:"seconds"`
	Err           string    `json:"err,omitempty"`
}

func main() {
	qPath := flag.String("questions", "", "FinanceBench questions JSONL")
	trees := flag.String("trees", "", "directory of tocdump trees")
	pdfs := flag.String("pdfs", "", "directory of filings")
	out := flag.String("out", "", "JSONL of per-question outcomes")
	maxLeaves := flag.Int("leaves", 3, "sections read per question")
	maxPages := flag.Int("pages", 40, "pages judged per question")
	limit := flag.Int("limit", 0, "stop after this many questions (0 = all)")
	flag.Parse()
	if *qPath == "" || *trees == "" || *pdfs == "" {
		fmt.Fprintln(os.Stderr, "usage: navbench -questions q.jsonl -trees dir -pdfs dir [-out o.jsonl]")
		os.Exit(2)
	}

	key := os.Getenv(typesafe.EnvAPIKey)
	tj, err := typesafe.New(typesafe.Config{APIKey: key})
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge:", err)
		os.Exit(1)
	}
	nav := &retrieval.JudgeNavigator{Judge: retry.NewJudge(retry.Config{MaxRetries: 3})(tj), MaxLeaves: *maxLeaves, MaxPages: *maxPages}

	qs := readQuestions(*qPath)
	if *limit > 0 && len(qs) > *limit {
		qs = qs[:*limit]
	}
	var of *os.File
	if *out != "" {
		of, _ = os.Create(*out)
		defer of.Close()
	}

	pageCache := map[string][]ingest.PageText{}
	leafCache := map[string][]retrieval.NavLeaf{}
	var results []outcome
	for _, q := range qs {
		o := outcome{ID: q.ID, Doc: q.Doc, Question: q.Question, Gold: q.Evidence}
		leaves, pages, err := load(q.Doc, *trees, *pdfs, leafCache, pageCache)
		if err != nil {
			o.Err = err.Error()
			results = append(results, o)
			report(o)
			continue
		}
		byNum := map[int]string{}
		for _, p := range pages {
			byNum[p.PageNumber] = p.Text
		}
		loadPages := func(_ context.Context, l retrieval.NavLeaf) ([]retrieval.NavPage, error) {
			var ps []retrieval.NavPage
			for n := l.Start; n <= l.End; n++ {
				if t, ok := byNum[n]; ok {
					ps = append(ps, retrieval.NavPage{Number: n, Text: t})
				}
			}
			return ps, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		start := time.Now()
		res, err := nav.Navigate(ctx, q.Question, leaves, loadPages)
		cancel()
		o.Seconds = time.Since(start).Seconds()
		if err != nil {
			o.Err = err.Error()
			results = append(results, o)
			report(o)
			continue
		}
		for _, l := range res.Selected {
			o.Selected = append(o.Selected, l.Title)
			o.SelectedPages = append(o.SelectedPages, [2]int{l.Start, l.End})
		}
		for _, e := range res.Evidence {
			o.Evidence = append(o.Evidence, e.Page.Number)
			o.EvidenceP = append(o.EvidenceP, e.P)
		}
		o.PagesRead = len(res.Pages)
		o.Requests, o.InTokens, o.CostUSD = res.Requests, res.Usage.InputTokens, res.Usage.CostUSD
		o.LeafHit = allInRanges(q.Evidence, o.SelectedPages)
		o.Recall = recall(q.Evidence, o.Evidence)
		o.Hit = o.Recall == 1
		results = append(results, o)
		report(o)
		if of != nil {
			b, _ := json.Marshal(o)
			of.Write(append(b, '\n'))
		}
	}
	summarise(results)
}

func load(doc, trees, pdfs string, leafCache map[string][]retrieval.NavLeaf, pageCache map[string][]ingest.PageText) ([]retrieval.NavLeaf, []ingest.PageText, error) {
	if l, ok := leafCache[doc]; ok {
		return l, pageCache[doc], nil
	}
	raw, err := os.ReadFile(filepath.Join(trees, doc+".json"))
	if err != nil {
		return nil, nil, err
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, err
	}
	if len(d.Nodes) == 0 {
		return nil, nil, fmt.Errorf("no tree")
	}
	f, err := os.Open(filepath.Join(pdfs, doc+".pdf"))
	if err != nil {
		return nil, nil, err
	}
	pd, err := parser.NewPDF().Parse(context.Background(), f)
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	pages := ingest.BenchAssemblePages(pd)
	var leaves []retrieval.NavLeaf
	var walk func(ns []tree.TOCNode, path string)
	walk = func(ns []tree.TOCNode, path string) {
		for _, n := range ns {
			p := n.Title
			if path != "" {
				p = path + " > " + n.Title
			}
			if len(n.Nodes) > 0 {
				walk(n.Nodes, p)
				continue
			}
			if n.StartPage > 0 && n.EndPage >= n.StartPage {
				leaves = append(leaves, retrieval.NavLeaf{ID: n.NodeID, Title: n.Title, Path: p, Start: n.StartPage, End: n.EndPage})
			}
		}
	}
	walk(d.Nodes, "")
	leafCache[doc], pageCache[doc] = leaves, pages
	return leaves, pages, nil
}

func allInRanges(gold []int, ranges [][2]int) bool {
	for _, g := range gold {
		in := false
		for _, r := range ranges {
			if g >= r[0] && g <= r[1] {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return len(gold) > 0
}

func recall(gold, got []int) float64 {
	if len(gold) == 0 {
		return 0
	}
	set := map[int]bool{}
	for _, g := range got {
		set[g] = true
	}
	n := 0
	for _, g := range gold {
		if set[g] {
			n++
		}
	}
	return float64(n) / float64(len(gold))
}

func report(o outcome) {
	mark := "MISS"
	if o.Hit {
		mark = "hit "
	} else if o.LeafHit {
		mark = "leaf"
	}
	if o.Err != "" {
		mark = "err "
	}
	fmt.Printf("%s %-24s gold %-10v evidence %-16v read %2d  %d req  %5.1fs  $%.5f  %s\n",
		mark, o.Doc, o.Gold, o.Evidence, o.PagesRead, o.Requests, o.Seconds, o.CostUSD, strings.TrimSpace(o.Err))
}

func summarise(rs []outcome) {
	var n, hit, leaf, errs int
	var rec, secs, cost float64
	var pages, reqs, toks int
	for _, o := range rs {
		if o.Err != "" {
			errs++
			continue
		}
		n++
		if o.Hit {
			hit++
		}
		if o.LeafHit {
			leaf++
		}
		rec += o.Recall
		secs += o.Seconds
		cost += o.CostUSD
		pages += o.PagesRead
		reqs += o.Requests
		toks += o.InTokens
	}
	if n == 0 {
		fmt.Println("no questions answered")
		return
	}
	var secList []float64
	for _, o := range rs {
		if o.Err == "" {
			secList = append(secList, o.Seconds)
		}
	}
	sort.Float64s(secList)
	fmt.Printf("\nquestions %d (errors %d)\n", n, errs)
	fmt.Printf("  gold pages inside a selected section   %d / %d  (%.3f)\n", leaf, n, float64(leaf)/float64(n))
	fmt.Printf("  every gold page in the evidence set    %d / %d  (%.3f)\n", hit, n, float64(hit)/float64(n))
	fmt.Printf("  mean page recall                       %.3f\n", rec/float64(n))
	fmt.Printf("  pages read / question                  %.1f\n", float64(pages)/float64(n))
	fmt.Printf("  requests / question                    %.1f\n", float64(reqs)/float64(n))
	fmt.Printf("  input tokens / question                %d\n", toks/n)
	fmt.Printf("  seconds / question  mean %.1f  median %.1f\n", secs/float64(n), secList[len(secList)/2])
	fmt.Printf("  cost / question                        $%.5f   total $%.4f\n", cost/float64(n), cost)
}

func readQuestions(path string) []question {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	var qs []question
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var q question
		if json.Unmarshal(sc.Bytes(), &q) == nil {
			qs = append(qs, q)
		}
	}
	return qs
}

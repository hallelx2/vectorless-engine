// Command tocresolve re-resolves the pages of an already-extracted tree,
// on a Judge alone.
//
// It exists to validate the resolver (HAL-1367) against a real filing
// without re-running the 150-second generative extraction: take the tree
// tocdump produced, throw away its page numbers, and see how many the
// resolver gets back — and where it puts them.
//
//	go run ./cmd/tocresolve -pdf doc.pdf -tree trees-after/doc.json -out trees-resolved/
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hallelx2/llmgate/judge/typesafe"
	"github.com/hallelx2/llmgate/middleware/retry"

	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

type dump struct {
	Doc      string         `json:"doc"`
	Pages    int            `json:"pages"`
	Seconds  float64        `json:"seconds"`
	Requests int            `json:"requests"`
	InTokens int            `json:"in_tokens"`
	CostUSD  float64        `json:"cost_usd"`
	Nodes    []tree.TOCNode `json:"nodes"`
	Err      string         `json:"err,omitempty"`
}

func main() {
	pdf := flag.String("pdf", "", "the filing")
	treePath := flag.String("tree", "", "tocdump output for it")
	out := flag.String("out", "", "directory to write the resolved tree")
	verbose := flag.Bool("v", false, "print each leaf's candidate pages before asking")
	flag.Parse()
	if *pdf == "" || *treePath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: tocresolve -pdf f.pdf -tree t.json -out dir")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*treePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintln(os.Stderr, "tree:", err)
		os.Exit(1)
	}

	f, err := os.Open(*pdf)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	doc, err := parser.NewPDF().Parse(context.Background(), f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	pages := ingest.BenchAssemblePages(doc.Sections)

	key := os.Getenv(typesafe.EnvAPIKey)
	if key == "" {
		key = dotEnv(typesafe.EnvAPIKey)
	}
	tj, err := typesafe.New(typesafe.Config{APIKey: key})
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge:", err)
		os.Exit(1)
	}
	b := &ingest.TOCBuilder{Judge: retry.NewJudge(retry.Config{MaxRetries: 3})(tj)}

	before := countWithPage(d.Nodes)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	// Exclude the pages detection actually calls a table of contents —
	// exactly what Build does. A structural guess was tried here first
	// and excluded 30 of 88 pages on a 10-K, because every page carries
	// "Table of Contents" as a running header. Decisive exclusions come
	// from the model's answer, never from a heuristic.
	b.MinimalContext = true
	exclude, du, ok := ingest.BenchDetectTOC(ctx, b, pages, 20)
	if !ok {
		fmt.Fprintln(os.Stderr, "detection declined; resolving with no exclusions")
		exclude = nil
	}
	if *verbose {
		for _, c := range ingest.BenchCandidates(d.Nodes, pages, exclude) {
			t := c.Title
			if len(t) > 40 {
				t = t[:40]
			}
			fmt.Fprintf(os.Stderr, "  cand %-40s claimed=%-3d pages=%v\n", t, c.Claimed, c.Pages)
		}
	}
	usage, handled := ingest.BenchResolvePages(ctx, b, d.Nodes, pages, exclude)
	usage.LLMCalls += du.LLMCalls
	usage.InputTokens += du.InputTokens
	usage.CostUSD += du.CostUSD
	elapsed := time.Since(start)
	if !handled {
		fmt.Fprintln(os.Stderr, "resolver declined")
		os.Exit(1)
	}
	ingest.BenchFinalise(d.Nodes, pages)
	after := countWithPage(d.Nodes)

	fmt.Printf("%s: %d pages, %d leaves, contents pages excluded: %v\n", d.Doc, len(pages), countLeaves(d.Nodes), exclude)
	fmt.Printf("  leaves with a page  %d -> %d\n", before, after)
	fmt.Printf("  judge requests      %d\n", usage.LLMCalls)
	fmt.Printf("  tokens              %d\n", usage.InputTokens)
	fmt.Printf("  elapsed             %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  cost                $%.6f\n\n", usage.CostUSD)
	printLeaves(d.Nodes, 0)

	d.Seconds, d.Requests, d.InTokens, d.CostUSD = elapsed.Seconds(), usage.LLMCalls, usage.InputTokens, usage.CostUSD
	d.Err = ""
	_ = os.MkdirAll(*out, 0o755)
	of, err := os.Create(filepath.Join(*out, d.Doc+".json"))
	if err == nil {
		enc := json.NewEncoder(of)
		enc.SetIndent("", " ")
		_ = enc.Encode(d)
		of.Close()
	}
}

func countWithPage(ns []tree.TOCNode) int {
	n := 0
	for _, x := range ns {
		if len(x.Nodes) > 0 {
			n += countWithPage(x.Nodes)
		} else if x.StartPage > 0 {
			n++
		}
	}
	return n
}

func countLeaves(ns []tree.TOCNode) int {
	n := 0
	for _, x := range ns {
		if len(x.Nodes) == 0 {
			n++
		} else {
			n += countLeaves(x.Nodes)
		}
	}
	return n
}

func printLeaves(ns []tree.TOCNode, depth int) {
	for _, x := range ns {
		if len(x.Nodes) > 0 {
			printLeaves(x.Nodes, depth+1)
			continue
		}
		t := x.Title
		if len(t) > 46 {
			t = t[:46]
		}
		fmt.Printf("  %sp%3d-%-3d %s\n", strings.Repeat("  ", depth), x.StartPage, x.EndPage, t)
	}
}

func dotEnv(key string) string {
	dir, _ := os.Getwd()
	for i := 0; i < 6 && dir != ""; i++ {
		if f, err := os.Open(filepath.Join(dir, ".env")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && strings.TrimSpace(k) == key {
					f.Close()
					return strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
			f.Close()
		}
		if p := filepath.Dir(dir); p != dir {
			dir = p
		} else {
			break
		}
	}
	return ""
}

// Command tocdump runs the full TOC pipeline on each PDF and writes the
// resulting tree, with page ranges, as JSON — one file per document.
//
// It exists to feed the evidence-page accuracy check. FinanceBench gives a
// gold evidence page for every question; this produces the section tree
// those pages should fall inside. Join the two and you have the only
// accuracy number that matters for detection: not "did the model say what
// another model said", but "does the tree put the answer where it is".
//
// Detection and verification run on the Judge when one is configured;
// extraction is generative and runs on the chat model regardless.
//
//	go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out ~/.cache/vlbench/trees
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

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/judge/typesafe"
	"github.com/hallelx2/llmgate/middleware/retry"
	"github.com/hallelx2/llmgate/provider/anthropic"

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
	docs := flag.String("docs", "", "directory of PDFs")
	out := flag.String("out", "", "directory for per-document tree JSON")
	noJudge := flag.Bool("no-judge", false, "run detection/verification on the chat model instead")
	minimal := flag.Bool("minimal", false, "TOCBuilder.MinimalContext: prefilter + truncation + two-stage scan")
	// 300s, not the pipeline's 90s default: GLM's extraction call on the
	// z.ai gateway routinely exceeds 90s on a 100+ page filing, and a
	// timeout there silently drops the whole tree. Measured 2026-09-18.
	callTimeout := flag.Duration("timeout", 300*time.Second, "per LLM call timeout")
	flag.Parse()
	if *docs == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: tocdump -docs <dir> -out <dir> [-no-judge]")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client, err := buildClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat model (needed for extraction):", err)
		os.Exit(1)
	}
	var judge llmgate.Judge
	if !*noJudge {
		if judge, err = buildJudge(); err != nil {
			fmt.Fprintln(os.Stderr, "judge:", err)
			os.Exit(1)
		}
	}

	pdfs, _ := filepath.Glob(filepath.Join(*docs, "*.pdf"))
	for _, path := range pdfs {
		name := strings.TrimSuffix(filepath.Base(path), ".pdf")
		d := dump{Doc: name}

		pages, err := readPages(path)
		if err != nil {
			d.Err = "parse: " + err.Error()
			write(*out, d)
			fmt.Printf("  %-28s parse FAILED\n", name)
			continue
		}
		d.Pages = len(pages)

		b := &ingest.TOCBuilder{LLM: client, Judge: judge, LLMCallTimeout: *callTimeout, MinimalContext: *minimal}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		start := time.Now()
		nodes, usage, err := b.Build(ctx, pages)
		cancel()
		d.Seconds = time.Since(start).Seconds()
		d.Requests, d.InTokens, d.CostUSD = usage.LLMCalls, usage.InputTokens, usage.CostUSD
		if err != nil {
			d.Err = err.Error()
		}
		d.Nodes = nodes
		write(*out, d)

		leaves := countLeaves(nodes)
		fmt.Printf("  %-28s %4d pages  %6.1fs  %3d req  %3d leaves  $%.4f %s\n",
			name, d.Pages, d.Seconds, d.Requests, leaves, d.CostUSD, d.Err)
	}
}

func write(dir string, d dump) {
	f, err := os.Create(filepath.Join(dir, d.Doc+".json"))
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	_ = enc.Encode(d)
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

// The three helpers below duplicate cmd/ingestbench. Two bench commands
// sharing sixty lines is not yet worth an internal package; if a third
// appears, it is.

func buildJudge() (llmgate.Judge, error) {
	key := os.Getenv(typesafe.EnvAPIKey)
	if key == "" {
		key = dotEnv(typesafe.EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf("no %s", typesafe.EnvAPIKey)
	}
	j, err := typesafe.New(typesafe.Config{APIKey: key})
	if err != nil {
		return nil, err
	}
	return retry.NewJudge(retry.Config{MaxRetries: 3})(j), nil
}

func buildClient() (llmgate.Client, error) {
	get := func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return dotEnv(k)
	}
	key := get("VLE_LLM_ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("no VLE_LLM_ANTHROPIC_API_KEY")
	}
	c, err := anthropic.New(anthropic.Config{
		APIKey: key, BaseURL: get("VLE_LLM_ANTHROPIC_BASE_URL"), Model: get("VLE_LLM_ANTHROPIC_MODEL"),
	})
	if err != nil {
		return nil, err
	}
	// A per-minute rate limit needs backoff measured in tens of seconds,
	// not the default 500ms-doubling that tops out around 3.5s total.
	// Three retries at that pace against z.ai's [1302] "Rate limit reached
	// for requests" burned a whole 21-document run to 0-leaf trees on
	// 2026-09-18. Six retries from 5s, capped at 60s, rides out a window.
	return retry.New(retry.Config{
		MaxRetries: 6,
		BaseDelay:  5 * time.Second,
		MaxDelay:   60 * time.Second,
	})(c), nil
}

func readPages(path string) ([]ingest.PageText, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	doc, err := parser.NewPDF().Parse(context.Background(), f)
	if err != nil {
		return nil, err
	}
	return ingest.BenchAssemblePages(doc.Sections), nil
}

func dotEnv(key string) string {
	dir, _ := os.Getwd()
	for i := 0; i < 6 && dir != ""; i++ {
		if f, err := os.Open(filepath.Join(dir, ".env")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
					f.Close()
					return strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
			f.Close()
		}
		p := filepath.Dir(dir)
		if p == dir {
			break
		}
		dir = p
	}
	return ""
}

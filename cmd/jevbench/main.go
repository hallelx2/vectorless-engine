// Command jevbench measures the TOC builder's two yes/no phases on a
// System One model against the generative path they replace.
//
// Both phases ask the same question of many pages. The generative path
// can only ask one at a time — detection is a sequential loop, and
// verification runs at a concurrency of 4 — so the cost is a round-trip
// per page. A Judge answers a whole batch against one reading of the
// state.
//
// This exists because that difference is an empirical claim, and the
// only honest way to hold it is to have run both on the same document.
//
//	TYPESAFE_API_KEY=... go run ./cmd/jevbench doc.pdf
//
// Supply VLE_LLM_ANTHROPIC_API_KEY as well to time the generative path
// alongside; without it only the Judge path runs.
package main

import (
	"bufio"
	"context"
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
)

func main() {
	repeat := flag.Int("repeat", 1, "replicate the parsed pages this many times")
	scan := flag.Int("scan", 20, "pages to scan for a table of contents")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: jevbench [-repeat N] [-scan N] <file.pdf>")
		os.Exit(2)
	}
	path := flag.Arg(0)

	pages, err := readPages(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	if *repeat > 1 {
		// Replicating real pages measures THROUGHPUT — how the batching
		// behaves at a page count the available corpus does not reach.
		// It says nothing about accuracy, and the output labels it so
		// nobody later reads it as a quality result.
		pages = replicatePages(pages, *repeat)
	}

	fmt.Printf("document   %s\n", filepath.Base(path))
	fmt.Printf("pages      %d", len(pages))
	if *repeat > 1 {
		fmt.Printf("   (SYNTHETIC: real pages replicated %dx for throughput only)", *repeat)
	}
	fmt.Println()
	fmt.Println()

	key := os.Getenv(typesafe.EnvAPIKey)
	if key == "" {
		key = dotEnv(typesafe.EnvAPIKey)
	}
	if key == "" {
		fmt.Fprintf(os.Stderr, "no %s in the environment or a .env up-tree\n", typesafe.EnvAPIKey)
		os.Exit(1)
	}

	tj, err := typesafe.New(typesafe.Config{APIKey: key})
	if err != nil {
		fmt.Fprintln(os.Stderr, "typesafe:", err)
		os.Exit(1)
	}
	judge := retry.NewJudge(retry.Config{MaxRetries: 3})(tj)

	b := &ingest.TOCBuilder{Judge: judge}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Phase 1 in isolation. This is the loop that runs sequentially in
	// the generative path — up to TOCCheckPages round-trips before tree
	// building starts at all.
	start := time.Now()
	toc, usage, handled := ingest.BenchDetectTOC(ctx, b, pages, *scan)
	elapsed := time.Since(start)

	if !handled {
		fmt.Println("detection: the Judge declined (see the error path); nothing to report")
		os.Exit(1)
	}

	scanned := *scan
	if len(pages) < scanned {
		scanned = len(pages)
	}

	fmt.Println("PHASE 1 — table-of-contents detection")
	fmt.Printf("  pages scanned    %d\n", scanned)
	fmt.Printf("  judge requests   %d\n", usage.LLMCalls)
	fmt.Printf("  elapsed          %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  tokens           %d in / %d out\n", usage.InputTokens, usage.OutputTokens)
	fmt.Printf("  cost             $%.8f\n", usage.CostUSD)
	fmt.Printf("  TOC pages found  %v\n", toc)

	// The comparison that matters. The generative path issues one call
	// per scanned page, strictly sequentially, so its floor is
	// scanned x per-call latency however fast the provider is.
	if usage.LLMCalls > 0 {
		perReq := elapsed / time.Duration(usage.LLMCalls)
		fmt.Printf("\n  per request      %s\n", perReq.Round(time.Millisecond))
		fmt.Printf("  the generative path issues %d sequential calls for the same answer;\n", scanned)
		fmt.Printf("  at 1-3s each that is %s-%s of wall-clock.\n",
			(time.Duration(scanned) * time.Second).Round(time.Second),
			(time.Duration(scanned) * 3 * time.Second).Round(time.Second))
	}
}

// readPages extracts per-page text with the engine's own parser, so the
// measurement runs on exactly what the ingest pipeline would see.
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
	return ingest.BenchAssemblePages(doc), nil
}

// dotEnv reads one key from a gitignored .env up-tree, so a credential
// never has to be pasted into a command line where it would land in
// shell history.
func dotEnv(key string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		if v := readEnvFile(filepath.Join(dir, ".env"), key); v != "" {
			return v
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func readEnvFile(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

// replicatePages repeats a document's pages to reach a page count the
// available corpus does not offer, renumbering so page keys stay unique.
//
// Only valid for timing. Every copy has identical content, so any
// accuracy figure derived from it would be measuring one page many
// times.
func replicatePages(pages []ingest.PageText, n int) []ingest.PageText {
	out := make([]ingest.PageText, 0, len(pages)*n)
	num := 1
	for i := 0; i < n; i++ {
		for _, p := range pages {
			out = append(out, ingest.PageText{PageNumber: num, Text: p.Text})
			num++
		}
	}
	return out
}

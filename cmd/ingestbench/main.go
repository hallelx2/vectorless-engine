// Command ingestbench measures the ingestion pipeline's TOC phases on
// real 10-K filings, with and without a System One model, and streams
// the run as it happens.
//
// The claim under test is narrow and worth stating plainly: vectorless
// has been slow to ingest because every judgement it makes costs a
// round-trip to a chat model, and a long filing needs a lot of them.
// A Judge answers a batch of questions against one reading of the state,
// so the same judgements cost a couple of requests instead of dozens.
//
// Both arms run the same documents through the same parser and the same
// builder. The only difference is which model answers the yes/no
// questions, which is what makes the comparison worth anything.
//
//	TYPESAFE_API_KEY=... VLE_LLM_ANTHROPIC_API_KEY=... \
//	  go run ./cmd/ingestbench -docs ~/.cache/vlbench/financebench -out run.jsonl
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/judge/typesafe"
	"github.com/hallelx2/llmgate/middleware/retry"
	"github.com/hallelx2/llmgate/provider/anthropic"

	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
)

func main() {
	docs := flag.String("docs", "", "directory of PDFs")
	out := flag.String("out", "ingestbench.jsonl", "event stream output")
	scan := flag.Int("scan", 20, "pages scanned for a table of contents")
	par := flag.Int("parallel", 3, "documents processed concurrently")
	arms := flag.String("arms", "jev,generative", "comma-separated arms to run")
	flag.Parse()

	if *docs == "" {
		fmt.Fprintln(os.Stderr, "usage: ingestbench -docs <dir> [-out run.jsonl]")
		os.Exit(2)
	}

	pdfs, err := filepath.Glob(filepath.Join(*docs, "*.pdf"))
	if err != nil || len(pdfs) == 0 {
		fmt.Fprintf(os.Stderr, "no PDFs in %s\n", *docs)
		os.Exit(1)
	}
	sort.Strings(pdfs)

	em, err := newEmitter(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open output:", err)
		os.Exit(1)
	}
	defer em.close()

	wanted := map[string]bool{}
	for _, a := range strings.Split(*arms, ",") {
		wanted[strings.TrimSpace(a)] = true
	}

	judge, jerr := buildJudge()
	needJudge := wanted["jev"] || wanted["jev-min"] || wanted["jev-fanout"]
	if needJudge && jerr != nil {
		fmt.Fprintln(os.Stderr, "judge:", jerr)
		os.Exit(1)
	}
	client, cerr := buildClient()
	if wanted["generative"] && cerr != nil {
		fmt.Fprintf(os.Stderr, "generative arm unavailable (%v); running jev only\n", cerr)
		delete(wanted, "generative")
	}

	em.emit(event{T: "run_start", Note: fmt.Sprintf("%d documents, parallel=%d, scan=%d",
		len(pdfs), *par, *scan)})
	fmt.Printf("documents %d · parallel %d · scan %d pages\n", len(pdfs), *par, *scan)
	fmt.Printf("streaming to %s\n\n", *out)

	// Parse once per document and share it across arms. Parsing is
	// identical work either way, and paying for it twice would inflate
	// both arms equally while adding minutes to the run.
	type parsed struct {
		name  string
		pages []ingest.PageText
	}
	var docsParsed []parsed

	for _, path := range pdfs {
		name := strings.TrimSuffix(filepath.Base(path), ".pdf")
		start := time.Now()
		pages, err := readPages(path)
		dur := time.Since(start)
		if err != nil {
			em.emit(event{T: "phase", Doc: name, Phase: "parse", Err: err.Error()})
			fmt.Printf("  %-28s parse FAILED: %v\n", name, err)
			continue
		}
		em.emit(event{T: "phase", Doc: name, Phase: "parse",
			Pages: len(pages), Seconds: dur.Seconds()})
		fmt.Printf("  %-28s parsed %4d pages in %6s\n", name, len(pages), dur.Round(time.Millisecond))
		docsParsed = append(docsParsed, parsed{name, pages})
	}
	fmt.Println()

	for _, arm := range []string{"jev", "jev-min", "jev-fanout", "generative"} {
		if !wanted[arm] {
			continue
		}
		fmt.Printf("=== arm: %s ===\n", arm)
		em.emit(event{T: "arm_start", Arm: arm})
		armStart := time.Now()

		// Documents run concurrently, as the real pipeline does. A
		// per-document timing alone would hide the thing that actually
		// matters to a queue: how many documents finish per minute.
		sem := make(chan struct{}, *par)
		var wg sync.WaitGroup

		for _, d := range docsParsed {
			wg.Add(1)
			go func(d parsed) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				runOne(em, arm, d.name, d.pages, *scan, judge, client)
			}(d)
		}
		wg.Wait()

		wall := time.Since(armStart)
		em.emit(event{T: "arm_end", Arm: arm, Seconds: wall.Seconds()})
		fmt.Printf("  arm wall-clock: %s\n\n", wall.Round(time.Millisecond))
	}

	em.emit(event{T: "run_end", Seconds: em.elapsed().Seconds()})
	fmt.Printf("total %s — events in %s\n", em.elapsed().Round(time.Second), *out)
}

// runOne times the detection phase for one document under one arm.
func runOne(em *emitter, arm, name string, pages []ingest.PageText, scan int,
	judge llmgate.Judge, client llmgate.Client) {

	b := &ingest.TOCBuilder{}
	if strings.HasPrefix(arm, "jev") {
		b.Judge = judge
		b.MinimalContext = arm == "jev-min"
	} else {
		b.LLM = client
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	em.emit(event{T: "phase_start", Doc: name, Arm: arm, Phase: "detect"})
	start := time.Now()

	var (
		found   []int
		usage   ingest.Usage
		handled bool
	)
	switch arm {
	case "jev", "jev-min":
		found, usage, handled = ingest.BenchDetectTOC(ctx, b, pages, scan)
	case "jev-fanout":
		found, usage, handled = ingest.BenchDetectTOCFanout(ctx, b, pages, scan)
	default:
		found, usage = ingest.BenchDetectTOCGenerative(ctx, b, pages, scan)
		handled = true
	}
	dur := time.Since(start)

	ev := event{
		T: "phase", Doc: name, Arm: arm, Phase: "detect",
		Seconds: dur.Seconds(), Requests: usage.LLMCalls,
		InTok: usage.InputTokens, OutTok: usage.OutputTokens,
		CostUSD: usage.CostUSD, Found: found,
	}
	if !handled {
		ev.Err = "judge declined; see the log line above"
	}
	em.emit(ev)

	fmt.Printf("  %-10s %-28s %8s  %2d req  %7d tok  $%.6f  toc=%v\n",
		arm, name, dur.Round(time.Millisecond), usage.LLMCalls,
		usage.InputTokens, usage.CostUSD, found)
}

func buildJudge() (llmgate.Judge, error) {
	key := os.Getenv(typesafe.EnvAPIKey)
	if key == "" {
		key = dotEnv(typesafe.EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf("no %s available", typesafe.EnvAPIKey)
	}
	j, err := typesafe.New(typesafe.Config{APIKey: key})
	if err != nil {
		return nil, err
	}
	return retry.NewJudge(retry.Config{MaxRetries: 3})(j), nil
}

func buildClient() (llmgate.Client, error) {
	key := os.Getenv("VLE_LLM_ANTHROPIC_API_KEY")
	if key == "" {
		key = dotEnv("VLE_LLM_ANTHROPIC_API_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("no VLE_LLM_ANTHROPIC_API_KEY available")
	}
	base := os.Getenv("VLE_LLM_ANTHROPIC_BASE_URL")
	if base == "" {
		base = dotEnv("VLE_LLM_ANTHROPIC_BASE_URL")
	}
	model := os.Getenv("VLE_LLM_ANTHROPIC_MODEL")
	if model == "" {
		model = dotEnv("VLE_LLM_ANTHROPIC_MODEL")
	}

	c, err := anthropic.New(anthropic.Config{APIKey: key, BaseURL: base, Model: model})
	if err != nil {
		return nil, err
	}
	return retry.New(retry.Config{MaxRetries: 3})(c), nil
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

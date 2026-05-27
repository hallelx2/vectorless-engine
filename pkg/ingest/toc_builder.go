package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// PageText pairs a 1-indexed PDF page number with its extracted
// text. The TOC builder reasons over a slice of these in page order
// — it never sees raw PDF bytes, so it works equally well over the
// pages produced by the existing parser pipeline and over synthetic
// fixtures used in tests.
type PageText struct {
	PageNumber int
	Text       string
}

// TOCBuilder builds an LLM-derived table-of-contents tree for a
// document. The shape mirrors PageIndex's three-phase pipeline:
//
//  1. detect    — scan the first TOCCheckPages pages and ask the LLM
//                 whether any of them looks like a real TOC.
//  2. extract   — if a TOC page was found, ask the LLM to parse it
//                 into structured nodes; otherwise call the no-TOC
//                 path that generates a TOC straight from body
//                 text (the LLM is given the full page text tagged
//                 with <physical_index_X> markers it copies back as
//                 the start page).
//  3. verify    — concurrently re-check each leaf node: does its
//                 title actually appear at the start of the claimed
//                 page? Mismatches are repaired by clearing the
//                 page back to zero; downstream readers treat zero
//                 as "open / unknown" rather than a wrong answer.
//
// EndPage is derived from sibling ordering once verification is
// done. The builder is deliberately tolerant of LLM parse blips
// (the same retry-then-degrade pattern the rest of the ingest path
// uses) — a single bad response never fails ingest.
type TOCBuilder struct {
	// LLM is the provider client. Required.
	LLM llmgate.Client

	// Model overrides the client's default. Empty inherits.
	Model string

	// Concurrency caps parallel LLM calls during the verification
	// phase. The detect + extract phases run sequentially because
	// each page-by-page detector call is short and the no-TOC
	// generator is one big call. Default: 4.
	Concurrency int

	// TOCCheckPages bounds the prefix the detector scans for a
	// table of contents. PageIndex defaults this to 20 — financial
	// filings put their TOC inside the first dozen pages and a
	// document with no TOC by page 20 almost never has one
	// further in. Default: 20.
	TOCCheckPages int
}

// Usage is the cumulative LLM accounting returned by Build. Mirrors
// the retrieval.Usage shape so callers can fold it into the same
// per-document cost ledger that the retrieval path uses.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	CostUSD      float64
	LLMCalls     int
}

// add folds the per-response usage from one LLM call into the
// running total. Keeps the call sites short.
func (u *Usage) add(r *llmgate.Response) {
	if r == nil {
		return
	}
	u.InputTokens += r.Usage.InputTokens
	u.OutputTokens += r.Usage.OutputTokens
	u.TotalTokens += r.Usage.TotalTokens
	u.CostUSD += r.Usage.CostUSD
	u.LLMCalls++
}

// Build runs the three-phase pipeline on pages and returns a
// flat-ish top-level TOC tree (children inside Nodes form the
// nested levels). Always returns a non-nil error chain only on a
// hard transport failure — LLM parse blips degrade to "empty
// result with logged warning" so the caller's ingest job never
// dies on a formatting glitch.
//
// pages must be in page order (PageNumber strictly ascending and
// 1-based). Build does not sort or de-duplicate.
func (b *TOCBuilder) Build(ctx context.Context, pages []PageText) ([]tree.TOCNode, Usage, error) {
	var usage Usage
	if len(pages) == 0 {
		return nil, usage, nil
	}
	concurrency := b.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	tocCheck := b.TOCCheckPages
	if tocCheck <= 0 {
		tocCheck = 20
	}

	// Phase 1: detect. Scan the leading pages for a TOC.
	tocPages := b.detectTOCPages(ctx, pages, tocCheck, &usage)

	// Phase 2: extract.
	var nodes []tree.TOCNode
	var err error
	if len(tocPages) > 0 {
		nodes, err = b.extractFromTOCPages(ctx, pages, tocPages, &usage)
	} else {
		nodes, err = b.generateNoTOC(ctx, pages, &usage)
	}
	if err != nil {
		return nil, usage, err
	}
	if len(nodes) == 0 {
		return nil, usage, nil
	}

	// Phase 3: verify each leaf's claimed start page actually
	// starts the section. Mismatches clear the page (set to 0)
	// rather than making one up — downstream treats zero as
	// open/unknown.
	b.verifyTitlesConcurrent(ctx, nodes, pages, concurrency, &usage)

	// Derive end pages from sibling order. Done last so verified
	// start pages drive the derivation.
	deriveEndPages(nodes, lastPage(pages))

	// Stamp stable node IDs onto every node so callers / external
	// consumers have an opaque handle independent of position.
	stampNodeIDs(nodes, "")

	return nodes, usage, nil
}

// detectTOCPages scans the first tocCheck pages with the
// PageIndex-style single-page detector. Returns the 1-indexed page
// numbers (in order) the LLM judged as table-of-contents pages.
//
// Detection failures (transport / parse) silently fall back to
// "no TOC found here" so the caller transitions to the no-TOC path.
// This matches the PageIndex contract — the no-TOC generator is
// strictly more general than the TOC-extraction path.
func (b *TOCBuilder) detectTOCPages(ctx context.Context, pages []PageText, tocCheck int, usage *Usage) []int {
	limit := tocCheck
	if limit > len(pages) {
		limit = len(pages)
	}
	var found []int
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return found
		}
		page := pages[i]
		text := strings.TrimSpace(page.Text)
		if text == "" {
			continue
		}
		isTOC, err := b.runTOCDetector(ctx, text, usage)
		if err != nil {
			// Transport / ErrNotImplemented — abandon detection and
			// let the caller fall back to the no-TOC path.
			return found
		}
		if isTOC {
			found = append(found, page.PageNumber)
		}
	}
	return found
}

// runTOCDetector asks the LLM whether the supplied page text reads
// like a table of contents. Mirrors PageIndex's
// toc_detector_single_page.
func (b *TOCBuilder) runTOCDetector(ctx context.Context, pageText string, usage *Usage) (bool, error) {
	prompt := fmt.Sprintf(`Your job is to detect if there is a table of contents provided in the given text.

Given text: %s

return the following JSON format:
{
    "thinking": "<why do you think there is a table of contents in the given text>",
    "toc_detected": "<yes or no>"
}

Directly return the final JSON structure. Do not output anything else.
Please note: abstract, summary, notation list, figure list, table list, etc. are not tables of contents.`, truncate(pageText, tocDetectorMaxChars))

	req := llmgate.Request{
		Model:       b.Model,
		Temperature: 0.0,
		MaxTokens:   400,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: tocDetectorSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		JSONMode:   true,
		JSONSchema: []byte(tocDetectorJSONSchema),
	}
	raw, err := runTOCJSONWithRetry(ctx, b.LLM, req, defaultTOCRetries, usage)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	var p tocDetectorPayload
	if err := unmarshalLenient([]byte(raw), &p); err != nil {
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(p.TOCDetected), "yes"), nil
}

// extractFromTOCPages joins the detected TOC pages and asks the
// LLM to parse them into structured nodes. The path used when a
// TOC page was found — the structure on the page is the structure
// the LLM is asked to reproduce, just with start_page resolved.
//
// On parse failure or transport blip, returns nil — the caller
// degrades to an empty tree (still useful: the document remains
// retrievable via the existing sections tree).
func (b *TOCBuilder) extractFromTOCPages(ctx context.Context, pages []PageText, tocPages []int, usage *Usage) ([]tree.TOCNode, error) {
	tocText := joinTOCPagesText(pages, tocPages)
	bodyText := buildPhysicalIndexedText(pages, tocDetectorMaxChars*4)

	prompt := fmt.Sprintf(`You are an expert in extracting hierarchical tree structure. Given a raw table-of-contents block and the document's body text (tagged with <physical_index_X> markers), produce the hierarchical TOC as a JSON array of nodes.

For each node:
- structure: dotted hierarchical index ("1", "1.1", "1.1.2") matching the heading depth.
- title: the original section title, only fixing space inconsistency.
- physical_index: the <physical_index_X> tag where the section begins. Look at the body text to resolve the page; if you cannot confidently locate it, use null.

Raw table of contents:
%s

Body text (with <physical_index_X> markers):
%s

Return ONLY a JSON object: {"nodes": [{"structure": "1", "title": "...", "physical_index": "<physical_index_3>"}, ...]}. Do not output anything else.`,
		truncate(tocText, tocExtractorMaxChars),
		truncate(bodyText, tocExtractorMaxBody),
	)

	req := llmgate.Request{
		Model:       b.Model,
		Temperature: 0.0,
		MaxTokens:   4096,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: tocExtractorSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		JSONMode:   true,
		JSONSchema: []byte(tocNodesJSONSchema),
	}
	raw, err := runTOCJSONWithRetry(ctx, b.LLM, req, defaultTOCRetries, usage)
	if err != nil {
		return nil, err
	}
	flat := parseTOCNodesPayload(raw)
	return assembleHierarchy(flat), nil
}

// generateNoTOC is the PageIndex-style process_no_toc driver: when
// no TOC page was found, page content (tagged with
// <physical_index_X> markers) is fed to the LLM with instructions
// to emit a TOC straight from headings in the body.
func (b *TOCBuilder) generateNoTOC(ctx context.Context, pages []PageText, usage *Usage) ([]tree.TOCNode, error) {
	body := buildPhysicalIndexedText(pages, noTOCMaxBody)
	prompt := fmt.Sprintf(`You are an expert in extracting hierarchical tree structure; your task is to generate the table-of-contents tree of the document below from its body text.

The structure variable is the dotted hierarchical index ("1", "1.1", "1.1.2") representing the section's position in the outline.

For the title, extract the original heading verbatim; only fix space inconsistency.

The text contains <physical_index_X> markers indicating the start and end of page X. For each section's physical_index, return the <physical_index_X> tag where the section starts (keep the format).

Body text:
%s

Return ONLY a JSON object: {"nodes": [{"structure": "1", "title": "...", "physical_index": "<physical_index_3>"}, ...]}. Do not output anything else.`, body)

	req := llmgate.Request{
		Model:       b.Model,
		Temperature: 0.0,
		MaxTokens:   4096,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: tocExtractorSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		JSONMode:   true,
		JSONSchema: []byte(tocNodesJSONSchema),
	}
	raw, err := runTOCJSONWithRetry(ctx, b.LLM, req, defaultTOCRetries, usage)
	if err != nil {
		return nil, err
	}
	flat := parseTOCNodesPayload(raw)
	return assembleHierarchy(flat), nil
}

// verifyTitlesConcurrent runs PageIndex's check_title_appearance_in_start
// over every node whose StartPage is set, with bounded concurrency.
// Mismatches set StartPage back to zero — the downstream contract
// is "zero means unknown / open" — so a misclaimed page never
// pretends to be authoritative.
func (b *TOCBuilder) verifyTitlesConcurrent(ctx context.Context, nodes []tree.TOCNode, pages []PageText, concurrency int, usage *Usage) {
	pageByNumber := indexByPage(pages)
	flat := flattenForVerify(nodes)
	if len(flat) == 0 {
		return
	}

	sem := make(chan struct{}, concurrency)
	g, gctx := errgroup.WithContext(ctx)
	var (
		mu       sync.Mutex
		localUse Usage
	)

	type result struct {
		node *tree.TOCNode
		ok   bool
	}
	results := make([]result, len(flat))

	for i, n := range flat {
		i, n := i, n
		if n.StartPage <= 0 {
			continue
		}
		pageText, ok := pageByNumber[n.StartPage]
		if !ok {
			// claimed a page we don't have — clear it.
			results[i] = result{node: n, ok: false}
			continue
		}
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-gctx.Done():
				return nil
			}
			startsHere, err := b.runVerifyTitleAtPageStart(gctx, n.Title, pageText, &localUse)
			if err != nil {
				// Transport / stub LLM — treat as "not verified" but
				// don't clear the page; the LLM never weighed in.
				results[i] = result{node: n, ok: true}
				return nil
			}
			results[i] = result{node: n, ok: startsHere}
			return nil
		})
	}
	_ = g.Wait()

	// Fold per-call usage into the caller's accumulator under the lock
	// so concurrent additions stay coherent.
	mu.Lock()
	usage.InputTokens += localUse.InputTokens
	usage.OutputTokens += localUse.OutputTokens
	usage.TotalTokens += localUse.TotalTokens
	usage.CostUSD += localUse.CostUSD
	usage.LLMCalls += localUse.LLMCalls
	mu.Unlock()

	for _, r := range results {
		if r.node == nil {
			continue
		}
		if !r.ok {
			r.node.StartPage = 0
		}
	}
}

// runVerifyTitleAtPageStart mirrors PageIndex's
// check_title_appearance_in_start: does this section's title appear
// at the beginning of the supplied page?
func (b *TOCBuilder) runVerifyTitleAtPageStart(ctx context.Context, title, pageText string, usage *Usage) (bool, error) {
	prompt := fmt.Sprintf(`You will be given a section title and a page's text.
Your job is to check if the section starts at the beginning of the given page text.
If there are other contents before the section title, then the section does NOT start at the beginning of the page text.
If the section title is the first meaningful content in the page text, then the section starts at the beginning.

Note: do fuzzy matching; ignore space inconsistency.

Section title: %s
Page text: %s

Reply format:
{
    "thinking": "<why you think the section appears or starts in the page text>",
    "start_begin": "<yes or no>"
}
Directly return the final JSON structure. Do not output anything else.`, title, truncate(pageText, verifyMaxChars))

	req := llmgate.Request{
		Model:       b.Model,
		Temperature: 0.0,
		MaxTokens:   400,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: tocVerifySystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		JSONMode:   true,
		JSONSchema: []byte(tocVerifyJSONSchema),
	}
	raw, err := runTOCJSONWithRetry(ctx, b.LLM, req, defaultTOCRetries, usage)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	var p tocVerifyPayload
	if err := unmarshalLenient([]byte(raw), &p); err != nil {
		// Couldn't parse — keep the page (don't clear). The LLM had
		// no clear say, so the safer move is "trust the extractor".
		return true, nil
	}
	return strings.EqualFold(strings.TrimSpace(p.StartBegin), "yes"), nil
}

// --- prompt + schema constants ---

const (
	tocDetectorSystemPrompt   = "You are a precise document-structure analyser. Decide whether a single page of text is a table of contents."
	tocExtractorSystemPrompt  = "You are an expert in extracting hierarchical tree structures from documents. You output strict JSON only."
	tocVerifySystemPrompt     = "You are a precise verifier. Decide whether a section title starts a page's text."
	defaultTOCRetries         = 2
	tocDetectorMaxChars       = 12000
	tocExtractorMaxChars      = 16000
	tocExtractorMaxBody       = 60000
	noTOCMaxBody              = 80000
	verifyMaxChars            = 4000
	tocDetectorJSONSchema     = `{"type":"object","properties":{"thinking":{"type":"string"},"toc_detected":{"type":"string"}},"required":["toc_detected"]}`
	tocVerifyJSONSchema       = `{"type":"object","properties":{"thinking":{"type":"string"},"start_begin":{"type":"string"}},"required":["start_begin"]}`
	tocNodesJSONSchema        = `{"type":"object","properties":{"nodes":{"type":"array","items":{"type":"object","properties":{"structure":{"type":"string"},"title":{"type":"string"},"physical_index":{"type":["string","null"]}},"required":["title"]}}},"required":["nodes"]}`
)

// --- JSON payload types ---

type tocDetectorPayload struct {
	Thinking    string `json:"thinking"`
	TOCDetected string `json:"toc_detected"`
}

type tocVerifyPayload struct {
	Thinking   string `json:"thinking"`
	StartBegin string `json:"start_begin"`
}

type tocNodePayload struct {
	Structure     string  `json:"structure"`
	Title         string  `json:"title"`
	PhysicalIndex *string `json:"physical_index"`
}

type tocNodesPayload struct {
	Nodes []tocNodePayload `json:"nodes"`
}

// --- shared helpers ---

// runTOCJSONWithRetry runs a JSON-mode TOC LLM call, retrying up to
// maxRetries additional times if the response can't be parsed.
// Mirrors the runSelectionWithRetry contract from
// pkg/retrieval/single_pass.go — copied here rather than imported
// because the retrieval package owns its own per-domain version of
// the same idea and we want the TOC builder to be importable by
// any future consumer without dragging retrieval in.
//
// Returns the final raw response text (empty on transport / stub
// failure). Caller decodes; a final parse failure degrades to "no
// usable response" rather than an error.
func runTOCJSONWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int, usage *Usage) (string, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastRaw string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req := baseReq
		if attempt > 0 {
			msgs := make([]llmgate.Message, len(baseReq.Messages))
			copy(msgs, baseReq.Messages)
			tail := len(msgs) - 1
			msgs[tail] = llmgate.Message{
				Role:    msgs[tail].Role,
				Content: msgs[tail].Content + "\n\nIMPORTANT: respond with ONLY a JSON object matching the schema. No prose, no markdown fences.",
			}
			req.Messages = msgs
		}
		resp, err := client.Complete(ctx, req)
		if err != nil {
			// Stub LLM (ErrNotImplemented) is a soft failure — the
			// caller will degrade. Transport errors do the same so
			// ingest never dies on a transient blip.
			if errors.Is(err, llmgate.ErrNotImplemented) {
				return "", nil
			}
			return "", err
		}
		usage.add(resp)
		lastRaw = resp.Content
		if looksLikeJSON(resp.Content) {
			return resp.Content, nil
		}
	}
	log.Printf("toc-builder: response did not parse after %d attempts; degrading to empty", maxRetries+1)
	return lastRaw, nil
}

// looksLikeJSON is a cheap probe so the retry loop can stop once
// the model returns something that at least textually resembles a
// JSON object. The real parser may still reject — strict parsing
// happens at the caller — but this avoids burning retries on
// obvious non-JSON ("Sure, here is the TOC: ...").
func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
	}
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

// unmarshalLenient strips code fences and any prose around the
// first { / last } before decoding, matching the parser pattern
// used in pkg/retrieval and pkg/ingest/summary_axes.go.
func unmarshalLenient(raw []byte, dst any) error {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	return json.Unmarshal([]byte(s), dst)
}

// parseTOCNodesPayload decodes the raw nodes JSON. Returns an empty
// slice on any parse failure — the builder caller treats "no
// usable nodes" as "leave TOC NULL" and proceeds with ingest.
func parseTOCNodesPayload(raw string) []tocNodePayload {
	if raw == "" {
		return nil
	}
	var p tocNodesPayload
	if err := unmarshalLenient([]byte(raw), &p); err != nil {
		return nil
	}
	return p.Nodes
}

// --- shape helpers ---

// joinTOCPagesText collects the text of the supplied TOC pages, in
// order, separated by newlines so the LLM sees them as one
// coherent block.
func joinTOCPagesText(pages []PageText, tocPages []int) string {
	idx := indexByPage(pages)
	var b strings.Builder
	for _, p := range tocPages {
		text, ok := idx[p]
		if !ok || text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// buildPhysicalIndexedText renders pages with <physical_index_X>
// markers around each page's text — the literal format the LLM is
// told to reproduce as the section's start page. budget caps the
// total characters so we never blow past the model's context.
func buildPhysicalIndexedText(pages []PageText, budget int) string {
	var b strings.Builder
	for _, p := range pages {
		seg := fmt.Sprintf("<physical_index_%d>\n%s\n<physical_index_%d>\n\n", p.PageNumber, p.Text, p.PageNumber)
		if budget > 0 && b.Len()+len(seg) > budget {
			break
		}
		b.WriteString(seg)
	}
	return b.String()
}

// indexByPage returns a map of page number to page text.
func indexByPage(pages []PageText) map[int]string {
	out := make(map[int]string, len(pages))
	for _, p := range pages {
		out[p.PageNumber] = p.Text
	}
	return out
}

// lastPage returns the highest PageNumber in pages, or zero if
// empty. Used as the default upper bound when deriving end pages.
func lastPage(pages []PageText) int {
	if len(pages) == 0 {
		return 0
	}
	last := pages[0].PageNumber
	for _, p := range pages[1:] {
		if p.PageNumber > last {
			last = p.PageNumber
		}
	}
	return last
}

// truncate caps s at max characters, appending an ellipsis when
// it had to cut. A non-positive max disables the cap.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// physicalIndexRE-like helper without regexp: parses the integer X
// out of "<physical_index_X>". Returns 0 when the input doesn't
// match — the verify phase treats zero as unknown.
func parsePhysicalIndex(s string) int {
	const prefix = "<physical_index_"
	const suffix = ">"
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return 0
	}
	mid := s[len(prefix) : len(s)-len(suffix)]
	n := 0
	for _, r := range mid {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// assembleHierarchy turns a flat list of TOC node payloads into a
// nested tree based on the dotted structure ("1", "1.1", "1.1.2").
// Missing intermediate parents are tolerated — orphans land at the
// top level so a misnumbered LLM response doesn't drop nodes
// silently.
func assembleHierarchy(flat []tocNodePayload) []tree.TOCNode {
	if len(flat) == 0 {
		return nil
	}
	// First materialise every payload as a TOCNode with its claimed
	// start page resolved.
	nodes := make([]tree.TOCNode, 0, len(flat))
	for _, n := range flat {
		title := strings.TrimSpace(n.Title)
		if title == "" {
			continue
		}
		page := 0
		if n.PhysicalIndex != nil {
			page = parsePhysicalIndex(*n.PhysicalIndex)
		}
		nodes = append(nodes, tree.TOCNode{
			Structure: strings.TrimSpace(n.Structure),
			Title:     title,
			StartPage: page,
		})
	}
	if len(nodes) == 0 {
		return nil
	}

	// Build a sentinel root; nest by counting dots in Structure.
	// "1" → depth 1, "1.2" → depth 2, "1.2.3" → depth 3.
	type ref struct {
		node      *tree.TOCNode
		structure string
	}
	var (
		out  []tree.TOCNode
		path []ref
	)
	for i := range nodes {
		n := &nodes[i]
		depth := depthOf(n.Structure)
		if depth <= 0 {
			depth = 1
		}
		// Pop the path stack down to depth-1 so a "1.2" inserts
		// under whatever last touched depth 1.
		for len(path) >= depth {
			path = path[:len(path)-1]
		}
		if len(path) == 0 {
			out = append(out, *n)
			path = append(path, ref{node: &out[len(out)-1], structure: n.Structure})
			continue
		}
		parent := path[len(path)-1].node
		parent.Nodes = append(parent.Nodes, *n)
		path = append(path, ref{node: &parent.Nodes[len(parent.Nodes)-1], structure: n.Structure})
	}
	return out
}

// depthOf returns the depth implied by a dotted structure string
// ("1" → 1, "1.2" → 2, "" → 0). A malformed structure ("1..2",
// "a.b") still returns the number of dot-separated tokens — we'd
// rather group than crash.
func depthOf(structure string) int {
	if structure == "" {
		return 0
	}
	return strings.Count(structure, ".") + 1
}

// flattenForVerify returns pointers to every node in the tree in
// depth-first pre-order so the verification phase can mutate
// StartPage in place.
func flattenForVerify(nodes []tree.TOCNode) []*tree.TOCNode {
	var out []*tree.TOCNode
	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for i := range ns {
			out = append(out, &ns[i])
			walk(ns[i].Nodes)
		}
	}
	walk(nodes)
	return out
}

// deriveEndPages walks the tree and fills each node's EndPage from
// the next sibling at the same depth (StartPage - 1) or the
// supplied docLastPage when no later sibling exists. Children's
// end pages cap at their parent's, which is what readers expect
// for a TOC.
func deriveEndPages(nodes []tree.TOCNode, docLastPage int) {
	deriveEndPagesIn(nodes, docLastPage)
}

func deriveEndPagesIn(nodes []tree.TOCNode, ceiling int) {
	for i := range nodes {
		n := &nodes[i]
		// Find the next sibling's start page that is strictly
		// greater than this one — that's our end. Skip sibling
		// entries whose StartPage was cleared (zero) by
		// verification so a single bad page doesn't sink the
		// rest of the row.
		end := 0
		for j := i + 1; j < len(nodes); j++ {
			if nodes[j].StartPage > n.StartPage {
				end = nodes[j].StartPage - 1
				break
			}
		}
		if end <= 0 {
			end = ceiling
		}
		// EndPage can never precede StartPage; clear to zero when
		// the data conflicts.
		if n.StartPage > 0 && end >= n.StartPage {
			n.EndPage = end
		}
		// Recurse with the child ceiling = this node's EndPage (or
		// the parent's ceiling if EndPage is unknown).
		childCeiling := n.EndPage
		if childCeiling == 0 {
			childCeiling = ceiling
		}
		deriveEndPagesIn(n.Nodes, childCeiling)
	}
}

// stampNodeIDs assigns deterministic NodeIDs based on the dotted
// structure (with a prefix), recursing into children. IDs are
// stable across runs given the same structure, which is handy for
// callers that diff trees across re-ingestions.
func stampNodeIDs(nodes []tree.TOCNode, prefix string) {
	for i := range nodes {
		n := &nodes[i]
		base := n.Structure
		if base == "" {
			base = fmt.Sprintf("n%d", i+1)
		}
		if prefix == "" {
			n.NodeID = "toc_" + base
		} else {
			n.NodeID = prefix + "_" + base
		}
		stampNodeIDs(n.Nodes, n.NodeID)
	}
}

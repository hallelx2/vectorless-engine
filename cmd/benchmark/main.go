// Command benchmark measures the latency difference between the
// vectorless server's REST (chi) endpoints and Connect-RPC (gRPC/HTTP)
// endpoints for the same operations.
//
// It hits a running server at --addr and tests:
//   - Health check
//   - List documents
//   - Get document tree
//   - Get section
//   - Query (LLM retrieval)
//
// Each operation is called N times over both REST and Connect-RPC.
// Results are printed as a comparison table.
//
// Usage:
//
//	go run ./cmd/benchmark --addr http://localhost:8080 --doc <document_id> --iterations 20
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/hallelx2/vectorless-engine/gen/vectorless/v1"
	"github.com/hallelx2/vectorless-engine/gen/vectorless/v1/vectorlessv1connect"
)

func main() {
	addr := flag.String("addr", "http://localhost:8080", "server base URL")
	docID := flag.String("doc", "", "document ID to benchmark against (required)")
	iterations := flag.Int("n", 20, "number of iterations per test")
	flag.Parse()

	if *docID == "" {
		fmt.Println("error: --doc is required")
		flag.Usage()
		return
	}

	base := strings.TrimRight(*addr, "/")
	n := *iterations
	ctx := context.Background()

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║        Vectorless Server — REST vs gRPC Benchmark          ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Server:     %-47s║\n", base)
	fmt.Printf("║  Document:   %-47s║\n", *docID)
	fmt.Printf("║  Iterations: %-47d║\n", n)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Build Connect-RPC clients.
	httpClient := &http.Client{Timeout: 30 * time.Second}
	healthClient := vectorlessv1connect.NewHealthServiceClient(httpClient, base)
	docsClient := vectorlessv1connect.NewDocumentsServiceClient(httpClient, base)
	queryClient := vectorlessv1connect.NewQueryServiceClient(httpClient, base)

	// First, grab the tree to get a section ID for the section benchmark.
	treeResp, err := restGET(base + "/v1/documents/" + *docID + "/tree")
	if err != nil {
		fmt.Printf("error fetching tree: %v\n", err)
		return
	}
	var treeData struct {
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	json.Unmarshal(treeResp, &treeData)
	sectionID := ""
	if len(treeData.Sections) > 1 {
		sectionID = treeData.Sections[1].ID // pick a child section
	} else if len(treeData.Sections) > 0 {
		sectionID = treeData.Sections[0].ID
	}
	fmt.Printf("Using section ID: %s\n\n", sectionID)

	type result struct {
		name     string
		rest     []time.Duration
		grpc     []time.Duration
	}

	var results []result

	// ── 1. Health Check ──────────────────────────────────────────
	fmt.Print("Benchmarking: Health Check ")
	r := result{name: "Health Check"}
	for i := 0; i < n; i++ {
		fmt.Print(".")
		// REST
		start := time.Now()
		restGET(base + "/v1/health")
		r.rest = append(r.rest, time.Since(start))

		// gRPC (Connect)
		start = time.Now()
		healthClient.Check(ctx, connect.NewRequest(&v1.HealthCheckRequest{}))
		r.grpc = append(r.grpc, time.Since(start))
	}
	fmt.Println(" done")
	results = append(results, r)

	// ── 2. List Documents ────────────────────────────────────────
	fmt.Print("Benchmarking: List Documents ")
	r = result{name: "List Documents"}
	for i := 0; i < n; i++ {
		fmt.Print(".")
		start := time.Now()
		restGET(base + "/v1/documents")
		r.rest = append(r.rest, time.Since(start))

		start = time.Now()
		docsClient.ListDocuments(ctx, connect.NewRequest(&v1.ListDocumentsRequest{Limit: 10}))
		r.grpc = append(r.grpc, time.Since(start))
	}
	fmt.Println(" done")
	results = append(results, r)

	// ── 3. Get Document Tree ─────────────────────────────────────
	fmt.Print("Benchmarking: Get Tree ")
	r = result{name: "Get Tree"}
	for i := 0; i < n; i++ {
		fmt.Print(".")
		start := time.Now()
		restGET(base + "/v1/documents/" + *docID + "/tree")
		r.rest = append(r.rest, time.Since(start))

		start = time.Now()
		docsClient.GetDocumentTree(ctx, connect.NewRequest(&v1.GetDocumentTreeRequest{
			DocumentId: *docID,
		}))
		r.grpc = append(r.grpc, time.Since(start))
	}
	fmt.Println(" done")
	results = append(results, r)

	// ── 4. Get Section ───────────────────────────────────────────
	if sectionID != "" {
		fmt.Print("Benchmarking: Get Section ")
		r = result{name: "Get Section"}
		for i := 0; i < n; i++ {
			fmt.Print(".")
			start := time.Now()
			restGET(base + "/v1/sections/" + sectionID)
			r.rest = append(r.rest, time.Since(start))

			start = time.Now()
			docsClient.GetSection(ctx, connect.NewRequest(&v1.GetSectionRequest{
				SectionId: sectionID,
			}))
			r.grpc = append(r.grpc, time.Since(start))
		}
		fmt.Println(" done")
		results = append(results, r)
	}

	// ── 5. Query (LLM retrieval) ─────────────────────────────────
	queries := []string{
		"What are the ownership rules in Rust?",
		"How does memory allocation work with the String type?",
		"What is the difference between move and clone?",
	}
	fmt.Print("Benchmarking: Query (3 queries x 2 protocols) ")
	r = result{name: "Query (LLM)"}
	for _, q := range queries {
		fmt.Print(".")
		// REST
		body, _ := json.Marshal(map[string]string{
			"document_id": *docID,
			"query":       q,
		})
		start := time.Now()
		restPOST(base+"/v1/query", body)
		r.rest = append(r.rest, time.Since(start))

		// gRPC (Connect)
		start = time.Now()
		queryClient.Query(ctx, connect.NewRequest(&v1.QueryRequest{
			DocumentId: *docID,
			Query:      q,
		}))
		r.grpc = append(r.grpc, time.Since(start))
	}
	fmt.Println(" done")
	results = append(results, r)

	// ── Print results ────────────────────────────────────────────
	fmt.Println()
	fmt.Println("╔════���══════════════╤════════════════╤════════════════╤═══════════╗")
	fmt.Println("║ Endpoint          │  REST (median) │  gRPC (median) │  Δ        ║")
	fmt.Println("╠═══════════════════╪════════════════╪════════════════╪═══════════╣")
	for _, r := range results {
		restMed := median(r.rest)
		grpcMed := median(r.grpc)
		delta := ""
		if restMed > 0 && grpcMed > 0 {
			if grpcMed < restMed {
				pct := float64(restMed-grpcMed) / float64(restMed) * 100
				delta = fmt.Sprintf("gRPC %.0f%% faster", pct)
			} else if restMed < grpcMed {
				pct := float64(grpcMed-restMed) / float64(grpcMed) * 100
				delta = fmt.Sprintf("REST %.0f%% faster", pct)
			} else {
				delta = "tied"
			}
		}
		fmt.Printf("║ %-17s │ %14s │ %14s │ %-9s ║\n",
			r.name,
			fmtDuration(restMed),
			fmtDuration(grpcMed),
			delta,
		)
	}
	fmt.Println("╚═══════════════════╧════════════════╧════════════════╧═══════════╝")

	// Detailed stats
	fmt.Println()
	fmt.Println("── Detailed Stats ──")
	for _, r := range results {
		fmt.Printf("\n  %s:\n", r.name)
		fmt.Printf("    REST  → min=%s  median=%s  p95=%s  max=%s  (n=%d)\n",
			fmtDuration(minD(r.rest)), fmtDuration(median(r.rest)),
			fmtDuration(percentile(r.rest, 95)), fmtDuration(maxD(r.rest)), len(r.rest))
		fmt.Printf("    gRPC  → min=%s  median=%s  p95=%s  max=%s  (n=%d)\n",
			fmtDuration(minD(r.grpc)), fmtDuration(median(r.grpc)),
			fmtDuration(percentile(r.grpc, 95)), fmtDuration(maxD(r.grpc)), len(r.grpc))
	}
}

// ── HTTP helpers ──────────────────────────────────────────────────

func restGET(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func restPOST(url string, body []byte) ([]byte, error) {
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ── Stats helpers ─────────────────────────────────────────────────

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

func percentile(ds []time.Duration, pct int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(pct) / 100.0 * float64(len(sorted)-1))
	return sorted[idx]
}

func minD(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	m := ds[0]
	for _, d := range ds[1:] {
		if d < m {
			m = d
		}
	}
	return m
}

func maxD(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	m := ds[0]
	for _, d := range ds[1:] {
		if d > m {
			m = d
		}
	}
	return m
}

func fmtDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

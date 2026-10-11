package pincite

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

type countingRaster struct{ chunks atomic.Int32 }

func (r *countingRaster) Available() bool { return true }
func (r *countingRaster) RenderPages(ctx context.Context, pdf []byte, from, to int, put func(int, []byte) error) error {
	r.chunks.Add(1)
	for p := from; p <= to; p++ {
		if err := put(p, []byte("jpeg")); err != nil {
			return err
		}
	}
	return nil
}

// Warm stops at the first chunk after the document is deleted, so a
// delete during warm-up is not followed by fresh page images.
func TestWarmStopsForDeletedDocument(t *testing.T) {
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := Source{DocumentID: "doc-gone", SourceRef: "documents/doc-gone/source.pdf", ContentType: "application/pdf"}
	if err := st.Put(ctx, src.SourceRef, strings.NewReader("%PDF-1.4"), storage.Metadata{}); err != nil {
		t.Fatal(err)
	}
	pages := make([]Page, warmChunk*3)
	for i := range pages {
		pages[i].Number = i + 1
	}
	raster := &countingRaster{}
	var checks atomic.Int32
	s := &Service{
		Storage: st, Raster: raster,
		Build: func([]byte) (*Layout, error) { return &Layout{Pages: pages}, nil },
		// Alive for the first chunk only: the delete lands mid warm-up.
		Alive: func(context.Context, string) bool { return checks.Add(1) == 1 },
	}
	if err := s.Warm(ctx, src); !errors.Is(err, ErrDocumentGone) {
		t.Fatalf("Warm = %v, want ErrDocumentGone", err)
	}
	if got := raster.chunks.Load(); got != 1 {
		t.Fatalf("rendered %d chunks, want 1 (stop once the document is gone)", got)
	}
	if ok, _ := st.Exists(ctx, PageKey(src.DocumentID, warmChunk+1)); ok {
		t.Fatal("a page after the delete was written")
	}
}

package pincite

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// ErrNoGeometry is returned for a source that has no page geometry —
// anything that is not a PDF. Callers degrade to section-level
// pincites; it is not a failure.
var ErrNoGeometry = errors.New("pincite: source has no page geometry")

// ErrNoRenderer is returned when page images are requested on a host
// with no rasterizer installed.
var ErrNoRenderer = errors.New("pincite: no page renderer available")

// Source identifies a document's original bytes.
type Source struct {
	DocumentID  string
	SourceRef   string // storage key of the original upload
	ContentType string
}

// IsPDF reports whether the source can carry page geometry.
func (s Source) IsPDF() bool {
	ct := strings.ToLower(s.ContentType)
	return strings.Contains(ct, "pdf") || strings.HasSuffix(strings.ToLower(s.SourceRef), ".pdf")
}

// Service builds, stores and serves a document's page layout and page
// images. Both are derived from the original upload once and kept in
// object storage beside it; the in-memory cache only saves re-reading
// a layout that is in active use.
type Service struct {
	Storage storage.Storage
	// Build extracts a layout from PDF bytes (parser.PDFLayout). Passed
	// in so this package does not depend on the parser.
	Build  func([]byte) (*Layout, error)
	Raster Rasterizer
	Logger *slog.Logger

	// CacheSize bounds how many layouts stay in memory. Default 8: a
	// long filing's layout is several MB decoded.
	CacheSize int

	// RenderSlots bounds concurrent renderer processes. Default: half
	// the CPUs, at least 2. Ingest warm-up and readers' on-demand pages
	// share the slots, so a reader's page interleaves with a warm-up
	// instead of competing with it for every core.
	RenderSlots int

	mu       sync.Mutex
	cache    map[string]*Layout
	order    []string
	group    singleflight.Group
	slotOnce sync.Once
	slots    chan struct{}
}

// warmChunk is how many pages one warm-up renderer call covers. Each
// chunk is stored as soon as it is rendered, so a freshly ingested
// document's first pages appear in seconds, not after the whole file.
const warmChunk = 12

// onDemandTimeout bounds one on-demand page render. It runs detached
// from the request, so a page the reader stopped waiting for is still
// stored for the next view.
const onDemandTimeout = 2 * time.Minute

func (s *Service) acquire(ctx context.Context) (func(), error) {
	s.slotOnce.Do(func() {
		n := s.RenderSlots
		if n <= 0 {
			n = max(2, runtime.NumCPU()/2)
		}
		s.slots = make(chan struct{}, n)
	})
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// LayoutKey and PageKey are where the derived artefacts live.
func LayoutKey(docID string) string {
	return fmt.Sprintf("pincites/%s/layout.v%d.json.gz", docID, LayoutVersion)
}

// PageKey is the storage key of one page image.
func PageKey(docID string, page int) string {
	return fmt.Sprintf("pincites/%s/pages/%d.jpg", docID, page)
}

// Layout returns the document's layout: from memory, else from
// storage, else built from the original and stored. Concurrent callers
// for the same document share one build.
func (s *Service) Layout(ctx context.Context, src Source) (*Layout, error) {
	if !src.IsPDF() {
		return nil, ErrNoGeometry
	}
	if l := s.cached(src.DocumentID); l != nil {
		return l, nil
	}
	v, err, _ := s.group.Do("layout:"+src.DocumentID, func() (any, error) {
		if l := s.cached(src.DocumentID); l != nil {
			return l, nil
		}
		if l, err := s.loadLayout(ctx, src.DocumentID); err == nil {
			s.remember(src.DocumentID, l)
			return l, nil
		}
		pdf, err := s.readSource(ctx, src)
		if err != nil {
			return nil, err
		}
		l, err := s.Build(pdf)
		if err != nil {
			return nil, err
		}
		if err := s.storeLayout(ctx, src.DocumentID, l); err != nil && s.Logger != nil {
			// Serving still works from memory; the next process rebuilds.
			s.Logger.Warn("pincite: store layout failed", "document_id", src.DocumentID, "err", err)
		}
		s.remember(src.DocumentID, l)
		return l, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Layout), nil
}

// PageImage returns page n's image. A page not yet rendered is rendered
// now, stored, and served; every later request reads the stored copy.
func (s *Service) PageImage(ctx context.Context, src Source, n int) ([]byte, error) {
	if !src.IsPDF() {
		return nil, ErrNoGeometry
	}
	if b, err := s.readKey(ctx, PageKey(src.DocumentID, n)); err == nil {
		return b, nil
	}
	if s.Raster == nil || !s.Raster.Available() {
		return nil, ErrNoRenderer
	}
	// Shared by every concurrent reader of this page, and detached from
	// any one request: the render finishes and is stored regardless.
	ch := s.group.DoChan(fmt.Sprintf("page:%s:%d", src.DocumentID, n), func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), onDemandTimeout)
		defer cancel()
		if b, err := s.readKey(rctx, PageKey(src.DocumentID, n)); err == nil {
			return b, nil
		}
		release, err := s.acquire(rctx)
		if err != nil {
			return nil, err
		}
		defer release()
		// The warm-up may have stored it while this waited for a slot.
		if b, err := s.readKey(rctx, PageKey(src.DocumentID, n)); err == nil {
			return b, nil
		}
		pdf, err := s.readSource(rctx, src)
		if err != nil {
			return nil, err
		}
		var out []byte
		err = s.Raster.RenderPages(rctx, pdf, n, n, func(page int, jpeg []byte) error {
			out = jpeg
			return s.Storage.Put(rctx, PageKey(src.DocumentID, page), bytes.NewReader(jpeg), storage.Metadata{ContentType: "image/jpeg"})
		})
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, fmt.Errorf("pincite: page %d not rendered", n)
		}
		return out, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]byte), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Warm builds the layout and renders every page, so the first reader
// never waits. Ingest calls it after a PDF is stored; it is idempotent,
// so re-ingest or a retry costs only the existence checks.
func (s *Service) Warm(ctx context.Context, src Source) error {
	if !src.IsPDF() {
		return nil
	}
	l, err := s.Layout(ctx, src)
	if err != nil {
		return fmt.Errorf("pincite warm: layout: %w", err)
	}
	if s.Raster == nil || !s.Raster.Available() {
		return nil
	}
	last := 0
	for _, p := range l.Pages {
		last = max(last, p.Number)
	}
	if last == 0 {
		return nil
	}
	if ok, _ := s.Storage.Exists(ctx, PageKey(src.DocumentID, last)); ok {
		return nil
	}
	pdf, err := s.readSource(ctx, src)
	if err != nil {
		return err
	}
	// In chunks, each stored as it lands and each holding one render
	// slot, so readers' pages interleave with the warm-up.
	for from := 1; from <= last; from += warmChunk {
		to := min(last, from+warmChunk-1)
		if ok, _ := s.Storage.Exists(ctx, PageKey(src.DocumentID, to)); ok {
			continue
		}
		release, err := s.acquire(ctx)
		if err != nil {
			return err
		}
		err = s.Raster.RenderPages(ctx, pdf, from, to, func(page int, jpeg []byte) error {
			return s.Storage.Put(ctx, PageKey(src.DocumentID, page), bytes.NewReader(jpeg), storage.Metadata{ContentType: "image/jpeg"})
		})
		release()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cached(id string) *Layout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[id]
}

func (s *Service) remember(id string, l *Layout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]*Layout{}
	}
	if _, ok := s.cache[id]; ok {
		return
	}
	limit := s.CacheSize
	if limit <= 0 {
		limit = 8
	}
	for len(s.order) >= limit {
		delete(s.cache, s.order[0])
		s.order = s.order[1:]
	}
	s.cache[id] = l
	s.order = append(s.order, id)
}

func (s *Service) loadLayout(ctx context.Context, id string) (*Layout, error) {
	b, err := s.readKey(ctx, LayoutKey(id))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var l Layout
	if err := json.NewDecoder(zr).Decode(&l); err != nil {
		return nil, err
	}
	if l.Version != LayoutVersion {
		return nil, fmt.Errorf("pincite: stale layout v%d", l.Version)
	}
	return &l, nil
}

func (s *Service) storeLayout(ctx context.Context, id string, l *Layout) error {
	// Gzipped: a 250-page filing's layout is ~5 MB of JSON and ~1 MB
	// compressed, and it is read whole on every cache miss.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(l); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return s.Storage.Put(ctx, LayoutKey(id), &buf, storage.Metadata{ContentType: "application/gzip"})
}

func (s *Service) readSource(ctx context.Context, src Source) ([]byte, error) {
	if src.SourceRef == "" {
		return nil, fmt.Errorf("pincite: document %s has no source", src.DocumentID)
	}
	return s.readKey(ctx, src.SourceRef)
}

func (s *Service) readKey(ctx context.Context, key string) ([]byte, error) {
	rc, _, err := s.Storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

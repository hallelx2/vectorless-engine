package retrieval

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// ErrNoPages is what a PageStore may return for a document that has no
// persisted pages, as an alternative to storage.ErrNotFound.
var ErrNoPages = errors.New("retrieval: document has no persisted pages")

// CorpusRef is a document a corpus question may read, as the caller's
// org- and store-scoped listing returned it. The library only builds
// documents it is handed, so the caller's scoping is the access control.
type CorpusRef struct {
	ID        tree.DocumentID
	Title     string
	UpdatedAt time.Time
	// TOC is the persisted contents tree, when ingest built one.
	TOC []byte
}

// CorpusLibrary turns document references into what corpus navigation
// reads — leaves, a page loader and a routing card — and keeps them in a
// byte-bounded cache keyed by document and its last update, so a
// collection's second question does not reload six hundred documents.
type CorpusLibrary struct {
	// Walk supplies the page sources: Pages for persisted per-page text,
	// PageLoader for section bodies.
	Walk *JudgeWalkStrategy
	// LoadTree reads a document's section tree, for documents without
	// persisted pages.
	LoadTree TreeLoader
	// CardChars bounds one routing card. Zero selects 900.
	CardChars int
	// BuildConcurrency bounds documents built at once. Zero selects 16.
	BuildConcurrency int
	// MaxBytes bounds the cache, counted as page text held. Zero selects
	// 512 MiB; a negative value disables caching.
	MaxBytes int64

	mu    sync.Mutex
	lru   *list.List
	items map[tree.DocumentID]*list.Element
	bytes int64
}

type corpusEntry struct {
	id      tree.DocumentID
	updated time.Time
	doc     CorpusDoc
	size    int64
}

const defaultCorpusCacheBytes = 512 << 20

// NewCorpusLibrary builds a library over a judgewalk strategy's page
// sources and a tree loader.
func NewCorpusLibrary(walk *JudgeWalkStrategy, loadTree TreeLoader) *CorpusLibrary {
	return &CorpusLibrary{Walk: walk, LoadTree: loadTree}
}

// Docs builds every referenced document, in the order given. A document
// that cannot be built is reported by ID and left out.
func (l *CorpusLibrary) Docs(ctx context.Context, orgID, storeID string, refs []CorpusRef) ([]CorpusDoc, map[tree.DocumentID]error) {
	out := make([]*CorpusDoc, len(refs))
	errs := map[tree.DocumentID]error{}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(corpusOr(l.BuildConcurrency, 16))
	for i, ref := range refs {
		g.Go(func() error {
			if d, ok := l.cached(ref); ok {
				out[i] = &d
				return nil
			}
			d, size, err := l.build(gctx, orgID, storeID, ref)
			if err != nil {
				mu.Lock()
				errs[ref.ID] = err
				mu.Unlock()
				return nil
			}
			l.store(ref, d, size)
			out[i] = &d
			return nil
		})
	}
	_ = g.Wait()
	docs := make([]CorpusDoc, 0, len(refs))
	for _, d := range out {
		if d != nil {
			docs = append(docs, *d)
		}
	}
	return docs, errs
}

// build reads one document's inputs: the persisted contents over the
// persisted pages when both exist, the pages alone as one leaf when only
// they exist, and the section tree otherwise.
func (l *CorpusLibrary) build(ctx context.Context, orgID, storeID string, ref CorpusRef) (CorpusDoc, int64, error) {
	var (
		in    *NavInputs
		pages []NavPage
	)
	if l.Walk != nil && l.Walk.Pages != nil {
		ps, err := l.Walk.Pages.LoadPages(ctx, ref.ID)
		switch {
		case err == nil:
			pages = ps
		case errors.Is(err, storage.ErrNotFound) || errors.Is(err, ErrNoPages):
			// No persisted pages: an unpaged document, read from its tree.
		default:
			// A storage failure is not "no pages": falling back would read
			// a paged document through its weaker section tree, silently.
			return CorpusDoc{}, 0, fmt.Errorf("corpus: load pages %s: %w", ref.ID, err)
		}
	}
	if len(ref.TOC) > 0 && len(pages) > 0 {
		in = PersistedNavInputs(ref.TOC, pages)
	}
	if in == nil && len(pages) > 0 {
		in = PagedNavInputs(ref.Title, pages)
	}
	if in == nil {
		if l.LoadTree == nil || l.Walk == nil {
			return CorpusDoc{}, 0, fmt.Errorf("corpus: %s has no pages and no tree loader", ref.ID)
		}
		t, err := l.LoadTree(ctx, ref.ID, orgID, storeID)
		if err != nil {
			return CorpusDoc{}, 0, fmt.Errorf("corpus: load tree %s: %w", ref.ID, err)
		}
		in = l.Walk.SectionTreeNavInputs(t)
		if in == nil {
			return CorpusDoc{}, 0, fmt.Errorf("corpus: %s has no navigable sections", ref.ID)
		}
	}

	var size int64
	opening := ""
	if in.Pages != nil {
		for _, p := range in.Pages {
			size += int64(len(p.Text))
		}
		opening = in.Pages[0].Text
	} else if ps, err := in.Load(ctx, in.Leaves[0]); err == nil && len(ps) > 0 {
		opening = ps[0].Text
	}
	cardChars := corpusOr(l.CardChars, defaultCorpusCardChars)
	if len(opening) > cardChars {
		opening = opening[:cardChars]
	}
	card := CorpusCard(ref.Title, in, opening, cardChars)
	size += int64(len(card))
	return CorpusDoc{ID: ref.ID, Title: ref.Title, Card: card, Inputs: in}, size, nil
}

func (l *CorpusLibrary) limit() int64 {
	if l.MaxBytes == 0 {
		return defaultCorpusCacheBytes
	}
	return l.MaxBytes
}

func (l *CorpusLibrary) cached(ref CorpusRef) (CorpusDoc, bool) {
	if l.limit() < 0 {
		return CorpusDoc{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.items == nil {
		return CorpusDoc{}, false
	}
	el, ok := l.items[ref.ID]
	if !ok {
		return CorpusDoc{}, false
	}
	e := el.Value.(*corpusEntry)
	if !e.updated.Equal(ref.UpdatedAt) { // re-ingested since: rebuild
		l.lru.Remove(el)
		delete(l.items, ref.ID)
		l.bytes -= e.size
		return CorpusDoc{}, false
	}
	l.lru.MoveToFront(el)
	return e.doc, true
}

func (l *CorpusLibrary) store(ref CorpusRef, d CorpusDoc, size int64) {
	limit := l.limit()
	if limit < 0 || size > limit {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.items == nil {
		l.items = map[tree.DocumentID]*list.Element{}
		l.lru = list.New()
	}
	if el, ok := l.items[ref.ID]; ok {
		l.bytes -= el.Value.(*corpusEntry).size
		l.lru.Remove(el)
	}
	l.items[ref.ID] = l.lru.PushFront(&corpusEntry{id: ref.ID, updated: ref.UpdatedAt, doc: d, size: size})
	l.bytes += size
	for l.bytes > limit && l.lru.Len() > 0 {
		back := l.lru.Back()
		e := back.Value.(*corpusEntry)
		l.lru.Remove(back)
		delete(l.items, e.id)
		l.bytes -= e.size
	}
}

// CachedDocuments reports how many documents and bytes the cache holds.
func (l *CorpusLibrary) CachedDocuments() (int, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lru == nil {
		return 0, 0
	}
	return l.lru.Len(), l.bytes
}

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/pincite"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// PincitesHandler serves the page geometry a reader's highlight needs
// (HAL-834): the document's page list and each page as an image. The
// regions themselves travel on the citations.
type PincitesHandler struct {
	logger *slog.Logger
	db     *db.Pool
	svc    *pincite.Service
}

// NewPincitesHandler creates a PincitesHandler. A nil service makes
// every route answer 501.
func NewPincitesHandler(logger *slog.Logger, pool *db.Pool, svc *pincite.Service) *PincitesHandler {
	return &PincitesHandler{logger: logger, db: pool, svc: svc}
}

// pinciteSource resolves a document's original upload for the caller's
// org and store.
func pinciteSource(ctx context.Context, pool *db.Pool, docID tree.DocumentID, orgID, storeID string) (pincite.Source, error) {
	doc, err := pool.GetDocument(ctx, docID, orgID, storeID)
	if err != nil {
		return pincite.Source{}, err
	}
	return pincite.Source{DocumentID: string(doc.ID), SourceRef: doc.SourceRef, ContentType: doc.ContentType}, nil
}

// HandleListPages returns the document's pages and their sizes:
//
//	{"document_id", "page_count", "pages": [{"page","width","height"}], "images": bool}
//
// width and height are PDF points, so a viewer can lay pages out at the
// right aspect before any image has loaded. A non-PDF document returns
// page_count 0: it has no pages to show, and pincites on it are
// section-level.
func (h *PincitesHandler) HandleListPages(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	if h.svc == nil {
		writeErr(w, http.StatusNotImplemented, "pincites are not configured on this server")
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	src, err := pinciteSource(r.Context(), h.db, id, orgID, storeID(r))
	if err != nil {
		writeDocErr(w, err)
		return
	}
	resp := map[string]any{"document_id": id, "page_count": 0, "pages": []pincite.PageSize{}, "images": false}
	if !src.IsPDF() {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	l, err := h.svc.Layout(r.Context(), src)
	if err != nil {
		h.logger.Error("pincites: layout", "document_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "page layout unavailable: "+err.Error())
		return
	}
	resp["page_count"] = len(l.Pages)
	resp["pages"] = l.Sizes()
	resp["images"] = h.svc.Raster != nil && h.svc.Raster.Available()
	writeJSON(w, http.StatusOK, resp)
}

// HandlePageImage returns page n as a JPEG. The bytes for a page never
// change once rendered, so the response is cacheable forever: the
// browser cache, not a revalidation round trip, serves every repeat
// view of a highlight.
func (h *PincitesHandler) HandlePageImage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	if h.svc == nil {
		writeErr(w, http.StatusNotImplemented, "pincites are not configured on this server")
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 1 {
		writeErr(w, http.StatusBadRequest, "page must be a positive integer")
		return
	}
	src, err := pinciteSource(r.Context(), h.db, id, orgID, storeID(r))
	if err != nil {
		writeDocErr(w, err)
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, src.DocumentID, n)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	l, err := h.svc.Layout(r.Context(), src)
	switch {
	case errors.Is(err, pincite.ErrNoGeometry):
		writeErr(w, http.StatusNotFound, "document has no pages to render")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "page layout unavailable: "+err.Error())
		return
	case l.Page(n) == nil:
		writeErr(w, http.StatusNotFound, "page out of range")
		return
	}
	img, err := h.svc.PageImage(r.Context(), src, n)
	if err != nil {
		if errors.Is(err, pincite.ErrNoRenderer) {
			writeErr(w, http.StatusNotImplemented, "page rendering is not available on this server")
			return
		}
		h.logger.Error("pincites: page image", "document_id", id, "page", n, "err", err)
		writeErr(w, http.StatusInternalServerError, "render page: "+err.Error())
		return
	}
	sum := sha256.Sum256(img)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-SHA256", hex.EncodeToString(sum[:8]))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img)
}

func writeDocErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// annotateRangeCitations adds page, regions and precision to citations
// that carry a quote and a page range (treewalk, store answer). The
// quote is located on the pages of its range; with no layout, or no
// quote, a citation is marked page- or section-level rather than left
// looking exact. Additive: start_page/end_page/quote are untouched.
func annotateRangeCitations(ctx context.Context, svc *pincite.Service, src pincite.Source, citations []map[string]any, logger *slog.Logger) {
	if len(citations) == 0 {
		return
	}
	var layout *pincite.Layout
	if svc != nil && src.IsPDF() {
		l, err := svc.Layout(ctx, src)
		if err != nil && logger != nil {
			logger.Warn("pincites: layout unavailable; citations stay page-level", "document_id", src.DocumentID, "err", err)
		}
		layout = l
	}
	for _, c := range citations {
		start, _ := c["start_page"].(int)
		end, _ := c["end_page"].(int)
		if end < start {
			end = start
		}
		quote, _ := c["quote"].(string)
		var pages []int
		for p := start; p > 0 && p <= end; p++ {
			pages = append(pages, p)
		}
		var m pincite.Match
		switch {
		case layout == nil && start > 0 && src.IsPDF():
			m = pincite.Match{Page: start, Precision: pincite.PrecisionPage}
		case layout == nil:
			m = pincite.Match{Precision: pincite.PrecisionSection}
		case quote == "":
			m = pincite.Resolve(layout, "", pages)
		default:
			m = pincite.Resolve(layout, quote, pages)
		}
		if m.Page > 0 {
			c["page"] = m.Page
		}
		c["regions"] = nonNilRegions(m.Regions)
		c["precision"] = m.Precision
	}
}

func nonNilRegions(r []pincite.Region) []pincite.Region {
	if r == nil {
		return []pincite.Region{}
	}
	return r
}

package ingest

import (
	"context"
	"errors"
	"path"

	"github.com/hallelx2/vectorless-engine/pkg/pincite"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// DocumentPrefixes lists every storage prefix a document writes under: the
// original upload and its section texts, the per-page text, and the rendered
// pincite pages and layout. Anything a document stores must fall under one of
// these, or deleting the document leaves it behind.
func DocumentPrefixes(id tree.DocumentID) []string {
	return []string{
		path.Join("documents", string(id)) + "/",
		PagesKey(id),
		pincite.DocPrefix(string(id)),
	}
}

// PurgeDocument removes every object a document stored. It tries all
// prefixes even when one fails, and returns how many objects it removed
// with the joined errors.
func PurgeDocument(ctx context.Context, st storage.Storage, id tree.DocumentID) (int, error) {
	if id == "" {
		return 0, storage.ErrEmptyPrefix
	}
	n := 0
	var errs []error
	for _, p := range DocumentPrefixes(id) {
		k, err := st.DeletePrefix(ctx, p)
		n += k
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

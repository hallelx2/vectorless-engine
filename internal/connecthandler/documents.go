package connecthandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"

	v1 "github.com/hallelx2/vectorless-engine/gen/vectorless/v1"
	"github.com/hallelx2/vectorless-engine/gen/vectorless/v1/vectorlessv1connect"
)

// orgIDFromConnect pulls the X-Vectorless-Org header that the control
// plane proxy injects on every authenticated SDK request. Missing
// header → InvalidArgument (the control plane should never let a
// request reach the engine without it; an empty header is a bug).
func orgIDFromConnect(req connect.AnyRequest) (string, error) {
	org := req.Header().Get("X-Vectorless-Org")
	if org == "" {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("missing X-Vectorless-Org header"))
	}
	return org, nil
}

// storeIDFromConnect pulls the X-Vectorless-Store header that the control
// plane proxy injects to scope a request to a single store. Unlike the
// org header it is optional: an empty value means "no store filter"
// (org-wide), which the db layer treats as a wildcard.
func storeIDFromConnect(req connect.AnyRequest) string {
	return req.Header().Get("X-Vectorless-Store")
}

// DocumentsService implements vectorlessv1connect.DocumentsServiceHandler.
type DocumentsService struct {
	vectorlessv1connect.UnimplementedDocumentsServiceHandler
	logger  *slog.Logger
	db      *db.Pool
	storage storage.Storage
	queue   queue.Queue
}

// NewDocumentsService creates a DocumentsService.
func NewDocumentsService(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	q queue.Queue,
) *DocumentsService {
	return &DocumentsService{
		logger:  logger,
		db:      pool,
		storage: store,
		queue:   q,
	}
}

// CreateDocument ingests a new document.
func (s *DocumentsService) CreateDocument(
	ctx context.Context,
	req *connect.Request[v1.CreateDocumentRequest],
) (*connect.Response[v1.CreateDocumentResponse], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	msg := req.Msg

	if len(msg.Content) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("content is required"))
	}

	docID := ingest.NewDocumentID()
	contentType := msg.ContentType
	if contentType == "" {
		contentType = guessContentType(msg.Filename)
	}

	key := ingest.SourceKey(docID, msg.Filename)
	reader := bytesReader(msg.Content)
	size := int64(len(msg.Content))

	if err := s.storage.Put(ctx, key, reader, storage.Metadata{
		Key: key, Size: size, ContentType: contentType,
	}); err != nil {
		s.logger.Error("ingest: storage put failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("storage write failed"))
	}

	title := msg.Filename
	if title == "" {
		title = string(docID)
	}

	if err := s.db.NewDocument(ctx, db.Document{
		ID:          docID,
		OrgID:       orgID,
		StoreID:     storeIDFromConnect(req),
		Title:       title,
		ContentType: contentType,
		SourceRef:   key,
		Status:      db.StatusPending,
		ByteSize:    size,
	}); err != nil {
		s.logger.Error("ingest: db insert failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("db write failed"))
	}

	payload, _ := json.Marshal(ingest.Payload{
		DocumentID:  docID,
		ContentType: contentType,
		Filename:    msg.Filename,
		SourceRef:   key,
	})
	if err := s.queue.Enqueue(ctx, queue.Job{
		Kind:      queue.KindIngestDocument,
		Payload:   payload,
		DedupeKey: string(docID),
	}); err != nil {
		s.logger.Error("ingest: enqueue failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("enqueue failed"))
	}

	return connect.NewResponse(&v1.CreateDocumentResponse{
		DocumentId: string(docID),
		Status:     string(db.StatusPending),
	}), nil
}

// GetDocument returns metadata for a document.
func (s *DocumentsService) GetDocument(
	ctx context.Context,
	req *connect.Request[v1.GetDocumentRequest],
) (*connect.Response[v1.Document], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	doc, err := s.db.GetDocument(ctx, tree.DocumentID(req.Msg.DocumentId), orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(docToProto(doc)), nil
}

// ListDocuments returns a paginated list.
func (s *DocumentsService) ListDocuments(
	ctx context.Context,
	req *connect.Request[v1.ListDocumentsRequest],
) (*connect.Response[v1.ListDocumentsResponse], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	opts := db.ListDocumentsOpts{
		OrgID:   orgID,
		StoreID: storeIDFromConnect(req),
		Limit:   int(msg.Limit),
		Status:  db.DocumentStatus(msg.Status),
	}
	if msg.Cursor != "" {
		if t, err := time.Parse(time.RFC3339Nano, msg.Cursor); err == nil {
			opts.Cursor = t
		}
	}

	docs, next, err := s.db.ListDocuments(ctx, opts)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	items := make([]*v1.Document, 0, len(docs))
	for i := range docs {
		items = append(items, docToProto(&docs[i]))
	}

	resp := &v1.ListDocumentsResponse{Documents: items}
	if !next.IsZero() {
		resp.NextCursor = next.Format(time.RFC3339Nano)
	}
	return connect.NewResponse(resp), nil
}

// DeleteDocument removes a document.
func (s *DocumentsService) DeleteDocument(
	ctx context.Context,
	req *connect.Request[v1.DeleteDocumentRequest],
) (*connect.Response[v1.DeleteDocumentResponse], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	if err := s.db.DeleteDocument(ctx, tree.DocumentID(req.Msg.DocumentId), orgID, storeIDFromConnect(req)); err != nil {
		if isNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.DeleteDocumentResponse{}), nil
}

// GetDocumentTree returns the tree view for LLM reasoning.
func (s *DocumentsService) GetDocumentTree(
	ctx context.Context,
	req *connect.Request[v1.GetDocumentTreeRequest],
) (*connect.Response[v1.DocumentTree], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	t, err := s.db.LoadTree(ctx, tree.DocumentID(req.Msg.DocumentId), orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	view := t.BuildView()
	sections := make([]*v1.SectionView, 0, len(view.Sections))
	for _, sv := range view.Sections {
		children := make([]string, len(sv.Children))
		for i, c := range sv.Children {
			children[i] = string(c)
		}
		sections = append(sections, &v1.SectionView{
			Id:       string(sv.ID),
			ParentId: string(sv.ParentID),
			Depth:    int32(sv.Depth),
			Title:    sv.Title,
			Summary:  sv.Summary,
			Children: children,
			Tokens:   int32(sv.Tokens),
		})
	}

	return connect.NewResponse(&v1.DocumentTree{
		DocumentId: string(view.DocumentID),
		Title:      view.Title,
		Sections:   sections,
	}), nil
}

// GetDocumentSource streams the original document bytes back to the caller.
func (s *DocumentsService) GetDocumentSource(
	ctx context.Context,
	req *connect.Request[v1.GetDocumentSourceRequest],
	stream *connect.ServerStream[v1.DocumentSourceChunk],
) error {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return err
	}
	docID := tree.DocumentID(req.Msg.DocumentId)
	doc, err := s.db.GetDocument(ctx, docID, orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	if doc.SourceRef == "" {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("document source not available"))
	}

	rc, _, err := s.storage.Get(ctx, doc.SourceRef)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("read source: %w", err))
	}
	defer rc.Close()

	// Stream in 32 KiB chunks.
	buf := make([]byte, 32*1024)
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&v1.DocumentSourceChunk{
				Data: buf[:n],
			}); sendErr != nil {
				return connect.NewError(connect.CodeInternal, sendErr)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return connect.NewError(connect.CodeInternal, readErr)
		}
	}
	return nil
}

// GetSection returns a single section with full content.
func (s *DocumentsService) GetSection(
	ctx context.Context,
	req *connect.Request[v1.GetSectionRequest],
) (*connect.Response[v1.Section], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	sec, err := s.db.GetSection(ctx, tree.SectionID(req.Msg.SectionId), orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	content := fetchContent(ctx, s.storage, sec.ContentRef)

	return connect.NewResponse(&v1.Section{
		Id:         string(sec.ID),
		DocumentId: string(sec.DocumentID),
		ParentId:   string(sec.ParentID),
		Ordinal:    int32(sec.Ordinal),
		Depth:      int32(sec.Depth),
		Title:      sec.Title,
		Summary:    sec.Summary,
		TokenCount: int32(sec.TokenCount),
		Metadata:   sec.Metadata,
		Content:    content,
	}), nil
}

// ── helpers ───────────────────────────────────────────────────────

func docToProto(d *db.Document) *v1.Document {
	return &v1.Document{
		Id:           string(d.ID),
		Title:        d.Title,
		ContentType:  d.ContentType,
		Status:       string(d.Status),
		ByteSize:     d.ByteSize,
		ErrorMessage: d.ErrorMessage,
		Metadata:     d.Metadata,
		CreatedAt:    timestamppb.New(d.CreatedAt),
		UpdatedAt:    timestamppb.New(d.UpdatedAt),
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, db.ErrNotFound)
}

func guessContentType(filename string) string {
	if filename == "" {
		return "application/octet-stream"
	}
	// Reuse a small subset — full detection happens in the parser.
	switch {
	case hasSuffix(filename, ".md"), hasSuffix(filename, ".markdown"):
		return "text/markdown"
	case hasSuffix(filename, ".txt"):
		return "text/plain"
	case hasSuffix(filename, ".html"), hasSuffix(filename, ".htm"):
		return "text/html"
	case hasSuffix(filename, ".pdf"):
		return "application/pdf"
	case hasSuffix(filename, ".docx"):
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	}
	return "application/octet-stream"
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

type bytesReaderImpl struct {
	data []byte
	pos  int
}

func bytesReader(b []byte) io.Reader {
	return &bytesReaderImpl{data: b}
}

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

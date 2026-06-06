package connecthandler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"

	v1 "github.com/hallelx2/vectorless-engine/gen/vectorless/v1"
	"github.com/hallelx2/vectorless-engine/gen/vectorless/v1/vectorlessv1connect"
)

// QueryService implements vectorlessv1connect.QueryServiceHandler.
type QueryService struct {
	vectorlessv1connect.UnimplementedQueryServiceHandler
	logger   *slog.Logger
	db       *db.Pool
	storage  storage.Storage
	strategy retrieval.Strategy
	multiDoc *retrieval.MultiDoc
}

// NewQueryService creates a QueryService.
func NewQueryService(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	strategy retrieval.Strategy,
	multiDoc *retrieval.MultiDoc,
) *QueryService {
	return &QueryService{
		logger:   logger,
		db:       pool,
		storage:  store,
		strategy: strategy,
		multiDoc: multiDoc,
	}
}

// Query runs the retrieval strategy and returns selected sections.
func (s *QueryService) Query(
	ctx context.Context,
	req *connect.Request[v1.QueryRequest],
) (*connect.Response[v1.QueryResponse], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	msg := req.Msg

	if msg.DocumentId == "" || msg.Query == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	if s.strategy == nil {
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}

	t, err := s.db.LoadTree(ctx, tree.DocumentID(msg.DocumentId), orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	budget := retrieval.ContextBudget{
		ModelName:         msg.Model,
		MaxTokens:         int(msg.MaxTokens),
		ReservedForPrompt: int(msg.ReservedForPrompt),
		MaxParallelCalls:  int(msg.MaxParallelCalls),
	}
	if budget.MaxTokens == 0 {
		budget.MaxTokens = 100000
	}
	if budget.ReservedForPrompt == 0 {
		budget.ReservedForPrompt = 4000
	}
	if budget.MaxParallelCalls == 0 {
		budget.MaxParallelCalls = 8
	}

	started := time.Now()
	ids, err := s.strategy.Select(ctx, t, msg.Query, budget)
	if err != nil {
		s.logger.Error("query: strategy failed", "err", err, "document_id", msg.DocumentId)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if msg.MaxSections > 0 && len(ids) > int(msg.MaxSections) {
		ids = ids[:msg.MaxSections]
	}

	sections := make([]*v1.QuerySection, 0, len(ids))
	for _, id := range ids {
		sec := t.FindByID(id)
		if sec == nil {
			continue
		}
		content := fetchContent(ctx, s.storage, sec.ContentRef)
		sections = append(sections, &v1.QuerySection{
			Id:         string(sec.ID),
			ParentId:   string(sec.ParentID),
			Title:      sec.Title,
			Summary:    sec.Summary,
			TokenCount: int32(sec.TokenCount),
			Content:    content,
		})
	}

	return connect.NewResponse(&v1.QueryResponse{
		DocumentId: msg.DocumentId,
		Query:      msg.Query,
		Strategy:   s.strategy.Name(),
		Model:      msg.Model,
		Sections:   sections,
		ElapsedMs:  time.Since(started).Milliseconds(),
	}), nil
}

// QueryMulti runs retrieval against multiple documents in parallel.
func (s *QueryService) QueryMulti(
	ctx context.Context,
	req *connect.Request[v1.QueryMultiRequest],
) (*connect.Response[v1.QueryMultiResponse], error) {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return nil, err
	}
	msg := req.Msg

	if len(msg.DocumentIds) == 0 || msg.Query == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("document_ids (non-empty) and query are required"))
	}
	if s.multiDoc == nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			fmt.Errorf("multi-document queries not configured"))
	}

	budget := retrieval.ContextBudget{
		ModelName:         msg.Model,
		MaxTokens:         int(msg.MaxTokens),
		ReservedForPrompt: int(msg.ReservedForPrompt),
		MaxParallelCalls:  int(msg.MaxParallelCalls),
	}
	if budget.MaxTokens == 0 {
		budget.MaxTokens = 100000
	}
	if budget.ReservedForPrompt == 0 {
		budget.ReservedForPrompt = 4000
	}
	if budget.MaxParallelCalls == 0 {
		budget.MaxParallelCalls = 8
	}

	docIDs := make([]tree.DocumentID, len(msg.DocumentIds))
	for i, id := range msg.DocumentIds {
		docIDs[i] = tree.DocumentID(id)
	}

	started := time.Now()
	result, err := s.multiDoc.Query(ctx, orgID, storeIDFromConnect(req), docIDs, msg.Query, budget)
	if err != nil {
		s.logger.Error("query/multi: failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	docs := make([]*v1.DocumentQueryResult, 0, len(result.Documents))
	for docID, dr := range result.Documents {
		sections := make([]*v1.QuerySection, 0, len(dr.SelectedIDs))
		for _, sid := range dr.SelectedIDs {
			sec := dr.Tree.FindByID(sid)
			if sec == nil {
				continue
			}
			content := fetchContent(ctx, s.storage, sec.ContentRef)
			sections = append(sections, &v1.QuerySection{
				Id:         string(sec.ID),
				ParentId:   string(sec.ParentID),
				Title:      sec.Title,
				Summary:    sec.Summary,
				TokenCount: int32(sec.TokenCount),
				Content:    content,
			})
			if msg.MaxSections > 0 && len(sections) >= int(msg.MaxSections) {
				break
			}
		}
		docs = append(docs, &v1.DocumentQueryResult{
			DocumentId: string(docID),
			Sections:   sections,
			Usage: &v1.QueryUsage{
				InputTokens:  int32(dr.Usage.InputTokens),
				OutputTokens: int32(dr.Usage.OutputTokens),
				TotalTokens:  int32(dr.Usage.TotalTokens),
				CostUsd:      dr.Usage.CostUSD,
				LlmCalls:     int32(dr.Usage.LLMCalls),
			},
		})
	}

	errs := make(map[string]string, len(result.Errors))
	for docID, e := range result.Errors {
		errs[string(docID)] = e.Error()
	}

	return connect.NewResponse(&v1.QueryMultiResponse{
		Query:     msg.Query,
		Strategy:  s.strategy.Name(),
		Model:     msg.Model,
		Documents: docs,
		Errors:    errs,
		ElapsedMs: time.Since(started).Milliseconds(),
		TotalUsage: &v1.QueryUsage{
			InputTokens:  int32(result.TotalUsage.InputTokens),
			OutputTokens: int32(result.TotalUsage.OutputTokens),
			TotalTokens:  int32(result.TotalUsage.TotalTokens),
			CostUsd:      result.TotalUsage.CostUSD,
			LlmCalls:     int32(result.TotalUsage.LLMCalls),
		},
	}), nil
}

// QueryMultiStream streams retrieval events across multiple documents.
func (s *QueryService) QueryMultiStream(
	ctx context.Context,
	req *connect.Request[v1.QueryMultiRequest],
	stream *connect.ServerStream[v1.QueryMultiStreamEvent],
) error {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return err
	}
	msg := req.Msg

	if len(msg.DocumentIds) == 0 || msg.Query == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("document_ids (non-empty) and query are required"))
	}
	if s.multiDoc == nil {
		return connect.NewError(connect.CodeUnimplemented,
			fmt.Errorf("multi-document queries not configured"))
	}

	budget := retrieval.ContextBudget{
		ModelName:         msg.Model,
		MaxTokens:         int(msg.MaxTokens),
		ReservedForPrompt: int(msg.ReservedForPrompt),
		MaxParallelCalls:  int(msg.MaxParallelCalls),
	}
	if budget.MaxTokens == 0 {
		budget.MaxTokens = 100000
	}
	if budget.ReservedForPrompt == 0 {
		budget.ReservedForPrompt = 4000
	}
	if budget.MaxParallelCalls == 0 {
		budget.MaxParallelCalls = 8
	}

	docIDs := make([]tree.DocumentID, len(msg.DocumentIds))
	for i, id := range msg.DocumentIds {
		docIDs[i] = tree.DocumentID(id)
	}

	events := s.multiDoc.QueryStream(ctx, orgID, storeIDFromConnect(req), docIDs, msg.Query, budget)

	for mevt := range events {
		evt := mevt.Event
		protoEvt := &v1.QueryMultiStreamEvent{
			DocumentId:  string(mevt.DocumentID),
			DocIndex:    int32(mevt.DocIndex),
			Type:        string(evt.Type),
			SliceIndex:  int32(evt.SliceIndex),
			TotalSlices: int32(evt.TotalSlices),
			Message:     evt.Message,
		}

		if evt.Type == retrieval.EventStarted {
			protoEvt.Query = msg.Query
		}
		if evt.Type == retrieval.EventError && evt.Error != nil {
			protoEvt.Message = evt.Error.Error()
		}

		if err := stream.Send(protoEvt); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
	}
	return nil
}

// fetchContent reads section content from storage. Returns empty string
// on any error — retrieval is best-effort for content.
func fetchContent(ctx context.Context, store storage.Storage, ref string) string {
	if ref == "" {
		return ""
	}
	rc, _, err := store.Get(ctx, ref)
	if err != nil {
		return ""
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close() // best-effort close
	return string(raw)
}

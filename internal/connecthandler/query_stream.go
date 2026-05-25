package connecthandler

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"

	v1 "github.com/hallelx2/vectorless-engine/gen/vectorless/v1"
)

// QueryStream implements the server-streaming RPC. Sections are sent
// to the client as the retrieval strategy identifies them, rather than
// waiting for the full run to complete.
func (s *QueryService) QueryStream(
	ctx context.Context,
	req *connect.Request[v1.QueryRequest],
	stream *connect.ServerStream[v1.QueryStreamEvent],
) error {
	orgID, err := orgIDFromConnect(req)
	if err != nil {
		return err
	}
	msg := req.Msg

	if msg.DocumentId == "" || msg.Query == "" {
		return connect.NewError(connect.CodeInvalidArgument, nil)
	}

	// Check if strategy supports streaming.
	ss, ok := s.strategy.(retrieval.StreamStrategy)
	if !ok {
		return connect.NewError(connect.CodeUnimplemented, nil)
	}

	t, err := s.db.LoadTree(ctx, tree.DocumentID(msg.DocumentId), orgID, storeIDFromConnect(req))
	if err != nil {
		if isNotFound(err) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewError(connect.CodeInternal, err)
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
	events := ss.SelectStream(ctx, t, msg.Query, budget)

	for evt := range events {
		protoEvt := &v1.QueryStreamEvent{
			Type:        string(evt.Type),
			SliceIndex:  int32(evt.SliceIndex),
			TotalSlices: int32(evt.TotalSlices),
			Message:     evt.Message,
		}

		switch evt.Type {
		case retrieval.EventStarted:
			protoEvt.Strategy = ss.Name()
			protoEvt.DocumentId = msg.DocumentId
			protoEvt.Query = msg.Query

		case retrieval.EventSectionSelected:
			sec := t.FindByID(evt.SectionID)
			if sec != nil {
				content := fetchContent(ctx, s.storage, sec.ContentRef)
				protoEvt.Section = &v1.QuerySection{
					Id:         string(sec.ID),
					ParentId:   string(sec.ParentID),
					Title:      sec.Title,
					Summary:    sec.Summary,
					TokenCount: int32(sec.TokenCount),
					Content:    content,
				}
			}

		case retrieval.EventCompleted:
			protoEvt.ElapsedMs = time.Since(started).Milliseconds()

		case retrieval.EventError:
			if evt.Error != nil {
				protoEvt.Message = evt.Error.Error()
			}
		}

		if err := stream.Send(protoEvt); err != nil {
			return err
		}
	}

	return nil
}

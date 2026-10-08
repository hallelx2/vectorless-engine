package retrieval

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// FallbackStrategy serves a request from Fallback when Primary's model
// provider refuses it outright: no credit (402), a revoked or rejected key
// (401/403), a gateway fault, or a provider outage (5xx after llmgate's
// own retries). It exists because on 2026-10-08 the Judge provider ran out
// of credit and every query on production failed for as long as it lasted
// (HAL-2478). A bad request, a canceled context or a context-length error
// is the caller's and is returned as it is: falling back would only repeat it.
//
// Every fallback logs at error level with the stable message
// FallbackLogMessage, which the production alert matches on.
type FallbackStrategy struct {
	Primary  Strategy
	Fallback Strategy
	Logger   *slog.Logger
}

// FallbackLogMessage is what each fallback logs; the alert policy keys on it.
const FallbackLogMessage = "retrieval: primary model unavailable, served by fallback"

// NewFallbackStrategy wraps primary so that fallback serves requests the
// primary's provider refuses.
func NewFallbackStrategy(primary, fallback Strategy, logger *slog.Logger) *FallbackStrategy {
	if logger == nil {
		logger = slog.Default()
	}
	return &FallbackStrategy{Primary: primary, Fallback: fallback, Logger: logger}
}

// Name is the primary's: the fallback is an availability measure, not a
// different product.
func (f *FallbackStrategy) Name() string { return f.Primary.Name() }

// ProviderUnavailable reports whether err is a model provider refusing
// service, as opposed to a problem with the request itself.
func ProviderUnavailable(err error) bool {
	var le *llmgate.LLMError
	if !errors.As(err, &le) {
		return false
	}
	switch {
	case le.StatusCode == 402, le.StatusCode == 401, le.StatusCode == 403:
		return true
	case le.StatusCode >= 500:
		return true
	}
	switch le.Class {
	case llmgate.ErrClassAuth, llmgate.ErrClassGateway:
		return true
	}
	return false
}

func (f *FallbackStrategy) fellBack(ctx context.Context, err error) {
	var le *llmgate.LLMError
	status := 0
	if errors.As(err, &le) {
		status = le.StatusCode
	}
	f.Logger.ErrorContext(ctx, FallbackLogMessage,
		"primary", f.Primary.Name(), "fallback", f.Fallback.Name(), "status", status, "err", err)
}

func (f *FallbackStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	ids, err := f.Primary.Select(ctx, t, query, budget)
	if err == nil || f.Fallback == nil || !ProviderUnavailable(err) || ctx.Err() != nil {
		return ids, err
	}
	f.fellBack(ctx, err)
	return f.Fallback.Select(ctx, t, query, budget)
}

func (f *FallbackStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	res, err := selectWithCost(ctx, f.Primary, t, query, budget)
	if err == nil || f.Fallback == nil || !ProviderUnavailable(err) || ctx.Err() != nil {
		return res, err
	}
	f.fellBack(ctx, err)
	return selectWithCost(ctx, f.Fallback, t, query, budget)
}

// WithSteps reports the primary's navigation steps; a request that falls
// back reports none from the fallback, which has no step stream.
func (f *FallbackStrategy) WithSteps(onStep func(NavStep)) Strategy {
	primary := f.Primary
	if ss, ok := primary.(StepStrategy); ok {
		primary = ss.WithSteps(onStep)
	}
	return &FallbackStrategy{Primary: primary, Fallback: f.Fallback, Logger: f.Logger}
}

func selectWithCost(ctx context.Context, s Strategy, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if cs, ok := s.(CostStrategy); ok {
		return cs.SelectWithCost(ctx, t, query, budget)
	}
	ids, err := s.Select(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return &Result{SelectedIDs: ids}, nil
}

// PublicError is the message a customer sees when retrieval fails. A
// provider's name, status and billing text stay in our logs: they are not
// the customer's business, and they name a supplier.
func PublicError(err error) string {
	if ProviderUnavailable(err) {
		return "retrieval is temporarily unavailable; please retry shortly"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "retrieval timed out; please retry"
	}
	return "retrieval failed"
}

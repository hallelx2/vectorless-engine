package retrieval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

type stubStrategy struct {
	name  string
	ids   []tree.SectionID
	err   error
	calls int
}

func (s *stubStrategy) Name() string { return s.name }
func (s *stubStrategy) Select(context.Context, *tree.Tree, string, ContextBudget) ([]tree.SectionID, error) {
	s.calls++
	return s.ids, s.err
}

func providerErr(status int) error {
	return fmt.Errorf("judgewalk: rank leaves: %w", &llmgate.LLMError{Provider: "typesafe", StatusCode: status, Message: "billing_error"})
}

func TestFallbackServesRequestsTheProviderRefuses(t *testing.T) {
	for _, status := range []int{402, 401, 403, 503} {
		var logs bytes.Buffer
		primary := &stubStrategy{name: "judgewalk", err: providerErr(status)}
		backup := &stubStrategy{name: "treewalk", ids: []tree.SectionID{"sec_1"}}
		f := NewFallbackStrategy(primary, backup, slog.New(slog.NewTextHandler(&logs, nil)))

		ids, err := f.Select(context.Background(), nil, "q", ContextBudget{})
		if err != nil || len(ids) != 1 || backup.calls != 1 {
			t.Fatalf("%d: want the fallback's answer, got ids=%v err=%v", status, ids, err)
		}
		res, err := f.SelectWithCost(context.Background(), nil, "q", ContextBudget{})
		if err != nil || res == nil || len(res.SelectedIDs) != 1 {
			t.Fatalf("%d: SelectWithCost must fall back too, got %+v err=%v", status, res, err)
		}
		if !strings.Contains(logs.String(), FallbackLogMessage) || !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("%d: every fallback logs the alert message at error level, got %q", status, logs.String())
		}
		if f.Name() != "judgewalk" {
			t.Fatalf("the wrapper keeps the primary's name, got %q", f.Name())
		}
	}
}

func TestFallbackLeavesTheCallersErrorsAlone(t *testing.T) {
	for name, err := range map[string]error{
		"bad request":  &llmgate.LLMError{Provider: "typesafe", StatusCode: 400, Class: llmgate.ErrClassBadRequest},
		"not an LLM":   errors.New("document has no sections"),
		"rate limited": &llmgate.LLMError{Provider: "typesafe", StatusCode: 429, Class: llmgate.ErrClassRateLimited},
	} {
		backup := &stubStrategy{name: "treewalk"}
		f := NewFallbackStrategy(&stubStrategy{name: "judgewalk", err: err}, backup, nil)
		if _, got := f.Select(context.Background(), nil, "q", ContextBudget{}); !errors.Is(got, err) || backup.calls != 0 {
			t.Fatalf("%s: returned as is, never retried on the fallback (err=%v calls=%d)", name, got, backup.calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backup := &stubStrategy{name: "treewalk"}
	f := NewFallbackStrategy(&stubStrategy{name: "judgewalk", err: providerErr(402)}, backup, nil)
	if _, err := f.Select(ctx, nil, "q", ContextBudget{}); err == nil || backup.calls != 0 {
		t.Fatal("a canceled request is not served by the fallback")
	}
}

func TestPublicErrorNamesNoProvider(t *testing.T) {
	for _, err := range []error{providerErr(402), providerErr(500), errors.New("typesafe exploded"), context.DeadlineExceeded} {
		msg := strings.ToLower(PublicError(err))
		for _, leak := range []string{"typesafe", "402", "billing", "judge", "credit"} {
			if strings.Contains(msg, leak) {
				t.Fatalf("%v: customer message %q mentions %q", err, msg, leak)
			}
		}
	}
}

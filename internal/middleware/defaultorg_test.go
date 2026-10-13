package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func orgSeen(t *testing.T, mw func(http.Handler) http.Handler, header string) string {
	t.Helper()
	var got string
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(OrgHeader)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/documents", nil)
	if header != "" {
		req.Header.Set(OrgHeader, header)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestDefaultOrgFillsMissingHeader(t *testing.T) {
	const nilOrg = "00000000-0000-0000-0000-000000000000"
	if got := orgSeen(t, DefaultOrg(nilOrg), ""); got != nilOrg {
		t.Fatalf("missing header: got %q, want %q", got, nilOrg)
	}
}

func TestDefaultOrgKeepsCallerOrg(t *testing.T) {
	if got := orgSeen(t, DefaultOrg("default"), "org-a"); got != "org-a" {
		t.Fatalf("explicit header overwritten: got %q", got)
	}
}

func TestDefaultOrgBlankHeaderCountsAsMissing(t *testing.T) {
	if got := orgSeen(t, DefaultOrg("default"), "   "); got != "default" {
		t.Fatalf("blank header: got %q, want default", got)
	}
}

func TestDefaultOrgEmptyConfigIsANoOp(t *testing.T) {
	if got := orgSeen(t, DefaultOrg("  "), ""); got != "" {
		t.Fatalf("empty default org set a header: %q", got)
	}
}

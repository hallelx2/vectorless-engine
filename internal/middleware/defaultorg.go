package middleware

import (
	"net/http"
	"strings"
)

// OrgHeader names the tenant a request belongs to. The control plane
// sets it on every request it forwards.
const OrgHeader = "X-Vectorless-Org"

// DefaultOrg gives requests that carry no OrgHeader the org org, so a
// single-tenant deployment without a control plane can be called with
// just its bearer token. A request that names an org keeps it. Runs
// after Auth, so only authenticated requests reach it when auth is on.
func DefaultOrg(org string) func(http.Handler) http.Handler {
	org = strings.TrimSpace(org)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if org != "" && strings.TrimSpace(r.Header.Get(OrgHeader)) == "" {
				r.Header.Set(OrgHeader, org)
			}
			next.ServeHTTP(w, r)
		})
	}
}

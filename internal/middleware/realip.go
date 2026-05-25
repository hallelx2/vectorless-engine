package middleware

import (
	"net"
	"net/http"
	"strings"
)

// RealIP sets r.RemoteAddr to the client's real IP when behind a
// trusted reverse proxy, by inspecting X-Forwarded-For and
// X-Real-IP headers.
//
// Order of precedence:
//  1. X-Real-IP (if set)
//  2. First IP in X-Forwarded-For
//  3. Original r.RemoteAddr (unchanged)
func RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			r.RemoteAddr = rip
			next.ServeHTTP(w, r)
			return
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// X-Forwarded-For may contain a comma-separated list:
			// "client, proxy1, proxy2". The first entry is the
			// original client.
			if i := strings.IndexByte(xff, ','); i > 0 {
				xff = xff[:i]
			}
			xff = strings.TrimSpace(xff)
			if ip := net.ParseIP(xff); ip != nil {
				r.RemoteAddr = xff
			}
		}
		next.ServeHTTP(w, r)
	})
}

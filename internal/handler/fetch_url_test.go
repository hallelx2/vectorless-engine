package handler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicOnly(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "64:ff9b::a00:1", "2002:a00:1::1"}
	for _, ip := range blocked {
		if err := publicOnly("tcp", net.JoinHostPort(ip, "443"), nil); err == nil {
			t.Errorf("%s: allowed, want blocked", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if err := publicOnly("tcp", net.JoinHostPort(ip, "443"), nil); err != nil {
			t.Errorf("%s: blocked (%v), want allowed", ip, err)
		}
	}
}

func TestFetchDocumentRefusesInternalServer(t *testing.T) {
	// A server on loopback stands in for the cloud metadata endpoint or an
	// internal service: the fetcher must never reach it.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()
	_, _, _, err := fetchDocument(context.Background(), srv.URL+"/doc.pdf")
	if !errors.Is(err, errBlockedAddress) {
		t.Fatalf("err = %v, want errBlockedAddress", err)
	}
	if hit {
		t.Fatal("the internal server was reached")
	}
}

func TestFetchDocumentRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"ftp://example.com/a.pdf", "file:///etc/passwd", "https://user:pw@example.com/a.pdf", "not a url", "https://"} {
		if _, _, _, err := fetchDocument(context.Background(), u); err == nil {
			t.Errorf("%q: accepted, want an error", u)
		}
	}
}

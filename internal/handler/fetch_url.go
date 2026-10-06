package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"
)

// maxFetchBytes caps a document fetched from a URL.
const maxFetchBytes = 100 << 20

var errBlockedAddress = errors.New("that address is not reachable from Vectorless")

// publicOnly refuses connections to loopback, private, link-local and
// other non-public addresses. It runs on the resolved IP at dial time,
// so a hostname that resolves (or re-resolves, or redirects) to an
// internal address is refused too.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() ||
		isSharedOrReserved(ip) {
		return errBlockedAddress
	}
	return nil
}

// isSharedOrReserved covers ranges IsPrivate leaves out: carrier-grade
// NAT (100.64/10), the IETF and benchmarking blocks, and 6to4/NAT64
// prefixes that can embed an internal IPv4 address.
func isSharedOrReserved(ip net.IP) bool {
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2002::/16"} {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// fetchClient downloads documents from caller-supplied URLs: public
// addresses only, at most five redirects, a bounded time.
var fetchClient = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return errors.New("redirect to an unsupported scheme")
		}
		return nil
	},
}

// fetchDocument downloads a document for ingest. It returns the bytes,
// a filename and the content type the server declared.
func fetchDocument(ctx context.Context, raw string) (body []byte, filename, contentType string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", "", errors.New(`"url" must be an http(s) URL`)
	}
	if u.User != nil {
		return nil, "", "", errors.New(`"url" must not carry credentials`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "Vectorless/1 (+https://vectorless.store)")
	resp, err := fetchClient.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddress) {
			return nil, "", "", errBlockedAddress
		}
		return nil, "", "", fmt.Errorf("could not fetch the URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("the URL answered HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxFetchBytes {
		return nil, "", "", fmt.Errorf("the document is larger than %d MB", maxFetchBytes>>20)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("could not read the URL: %w", err)
	}
	if len(body) > maxFetchBytes {
		return nil, "", "", fmt.Errorf("the document is larger than %d MB", maxFetchBytes>>20)
	}
	contentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if _, params, e := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); e == nil && params["filename"] != "" {
		filename = path.Base(params["filename"])
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = path.Base(resp.Request.URL.Path)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = resp.Request.URL.Hostname()
	}
	return body, filename, contentType, nil
}

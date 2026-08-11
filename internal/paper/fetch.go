package paper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// Size limits for remote documents.
const (
	MaxProviderBytes  = 5 * 1024 * 1024
	MaxPaperPageBytes = 2 * 1024 * 1024
)

// ImportError is a user-facing import failure.
type ImportError struct{ Reason string }

func (e *ImportError) Error() string { return e.Reason }

// NotFoundError reports that no usable metadata exists.
type NotFoundError struct{ Reason string }

func (e *NotFoundError) Error() string { return e.Reason }

// FetchError reports a provider or network failure. Transient failures are
// retried with backoff; permanent ones are not.
type FetchError struct {
	Reason    string
	Transient bool
}

func (e *FetchError) Error() string { return e.Reason }

// UnsafeURLError reports a URL that failed Herald's public-HTTPS checks.
type UnsafeURLError struct{ Reason string }

func (e *UnsafeURLError) Error() string { return e.Reason }

// InvalidInputError reports input the caller can correct, such as a malformed
// identifier. It is distinct from UnsafeURLError so the API can answer 400 for
// both while keeping the two causes separable.
type InvalidInputError struct{ Reason string }

func (e *InvalidInputError) Error() string { return e.Reason }

// IsInvalidInput reports whether err is a correctable input error.
func IsInvalidInput(err error) bool {
	var invalid *InvalidInputError
	return errors.As(err, &invalid)
}

// Error classification helpers used by the API layer to pick a status code.
func IsUnsafeURL(err error) bool { var t *UnsafeURLError; return errors.As(err, &t) }
func IsNotFound(err error) bool  { var t *NotFoundError; return errors.As(err, &t) }
func IsFetchError(err error) bool {
	var t *FetchError
	return errors.As(err, &t)
}
func IsImportError(err error) bool {
	var importErr *ImportError
	return errors.As(err, &importErr) || IsNotFound(err) ||
		IsFetchError(err) || IsUnsafeURL(err) || IsInvalidInput(err)
}

// Fetcher retrieves a remote document. It is an interface seam so tests never
// touch the network.
type Fetcher func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error)

// validatePublicHTTPSURL enforces every rule Herald applies before it will
// fetch a user-supplied URL.
//
// This is the SSRF boundary. A paper URL comes straight from user input, so it
// must be HTTPS, carry no credentials, use the standard port, and resolve only
// to globally routable addresses — otherwise it could be used to reach the
// loopback interface, link-local metadata services, or a private network.
func validatePublicHTTPSURL(rawURL string, resolveDNS bool) (string, error) {
	parts := urlx.Split(strings.TrimSpace(rawURL))
	hostname := parts.Hostname()
	if strings.ToLower(parts.Scheme) != "https" || hostname == "" {
		return "", &UnsafeURLError{Reason: "Paper URLs must use public HTTPS"}
	}
	if parts.HasUserinfo() {
		return "", &UnsafeURLError{Reason: "Paper URLs cannot contain credentials"}
	}
	port, present, err := parts.Port()
	if err != nil {
		return "", &UnsafeURLError{Reason: "Paper URL has an invalid port"}
	}
	if present && port != 443 {
		return "", &UnsafeURLError{Reason: "Paper URLs must use the standard HTTPS port"}
	}

	var addresses []net.IP
	if literal := net.ParseIP(hostname); literal != nil {
		addresses = append(addresses, literal)
	} else if resolveDNS {
		resolved, err := net.LookupIP(hostname)
		if err != nil {
			return "", &FetchError{
				Reason:    fmt.Sprintf("Could not resolve paper host: %v", err),
				Transient: true,
			}
		}
		addresses = resolved
	}
	for _, address := range addresses {
		if !isGlobalUnicast(address) {
			return "", &UnsafeURLError{
				Reason: "Paper URLs cannot target private or local networks",
			}
		}
	}

	path := parts.Path
	if path == "" {
		path = "/"
	}
	return urlx.Unsplit(urlx.Parts{
		Scheme: "https", Netloc: parts.Netloc, Path: path, Query: parts.Query,
	}), nil
}

// isGlobalUnicast reports whether an address is publicly routable.
//
// It mirrors Python's ipaddress.is_global: loopback, link-local, private,
// multicast, unspecified, and the IPv6 unique-local range are all rejected.
func isGlobalUnicast(address net.IP) bool {
	if address.IsLoopback() || address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() || address.IsInterfaceLocalMulticast() ||
		address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	if v4 := address.To4(); v4 != nil {
		switch {
		case v4[0] == 10,
			v4[0] == 172 && v4[1]&0xf0 == 16,
			v4[0] == 192 && v4[1] == 168,
			v4[0] == 127,
			v4[0] == 100 && v4[1]&0xc0 == 64, // 100.64.0.0/10 carrier-grade NAT
			v4[0] == 192 && v4[1] == 0 && v4[2] == 0,
			v4[0] == 192 && v4[1] == 0 && v4[2] == 2,
			v4[0] == 198 && v4[1]&0xfe == 18,
			v4[0] == 198 && v4[1] == 51 && v4[2] == 100,
			v4[0] == 203 && v4[1] == 0 && v4[2] == 113,
			v4[0] >= 240, // reserved and broadcast
			v4[0] == 0:
			return false
		}
		return true
	}
	// IPv6 unique-local (fc00::/7) and documentation (2001:db8::/32).
	if len(address) == net.IPv6len {
		if address[0]&0xfe == 0xfc {
			return false
		}
		if address[0] == 0x20 && address[1] == 0x01 &&
			address[2] == 0x0d && address[3] == 0xb8 {
			return false
		}
	}
	return true
}

// FetchPublicDocument retrieves a document over public HTTPS, re-validating
// every redirect hop.
func FetchPublicDocument(rawURL string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
	safeURL, err := validatePublicHTTPSURL(rawURL, true)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: guardedTransport(),
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return &FetchError{Reason: "Too many redirects"}
			}
			// A redirect is attacker-influenced input, so it faces the same
			// checks as the original URL.
			if _, err := validatePublicHTTPSURL(request.URL.String(), true); err != nil {
				return err
			}
			return nil
		},
	}
	request, err := http.NewRequest(http.MethodGet, safeURL, nil)
	if err != nil {
		return nil, &FetchError{Reason: err.Error()}
	}
	request.Header.Set("User-Agent", "Herald/0.1 (+local research reader; polite metadata client)")
	request.Header.Set("Accept-Encoding", "identity")
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		var unsafe *UnsafeURLError
		if errors.As(err, &unsafe) {
			return nil, unsafe
		}
		return nil, &FetchError{
			Reason:    fmt.Sprintf("Could not fetch paper metadata: %v", err),
			Transient: true,
		}
	}
	defer response.Body.Close()

	if _, err := validatePublicHTTPSURL(response.Request.URL.String(), true); err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{Reason: "Paper metadata was not found"}
	}
	if response.StatusCode >= 400 {
		return nil, &FetchError{
			Reason: fmt.Sprintf("Metadata provider returned HTTP %d", response.StatusCode),
			Transient: response.StatusCode == http.StatusTooManyRequests ||
				response.StatusCode >= 500,
		}
	}
	// Reject an oversized body before reading it, when the server declares one.
	if declared := response.Header.Get("Content-Length"); declared != "" {
		if size, convErr := strconv.Atoi(declared); convErr == nil && size > maxBytes {
			return nil, &FetchError{Reason: "Remote metadata exceeds Herald's size limit"}
		}
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, &FetchError{
			Reason:    fmt.Sprintf("Could not fetch paper metadata: %v", err),
			Transient: true,
		}
	}
	if len(document) > maxBytes {
		return nil, &FetchError{Reason: "Remote metadata exceeds Herald's size limit"}
	}
	return document, nil
}

// guardedTransport re-checks the resolved address at connect time.
//
// Validating the hostname up front is not sufficient on its own: between that
// lookup and the connection, DNS can return a different answer, so a name that
// resolved publicly could still be dialed against a loopback or private
// address. Checking again here closes that window.
func guardedTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil && !isGlobalUnicast(ip) {
			return nil, &UnsafeURLError{
				Reason: "Paper URLs cannot target private or local networks",
			}
		}
		return dialer.DialContext(ctx, network, address)
	}
	return transport
}

// Package urlx provides the small set of URL primitives Herald relies on,
// with the same semantics the original implementation inherited from Python's
// urllib.parse. Keeping them in one place makes the compatibility contract
// explicit instead of scattering subtly different rules across packages.
package urlx

import (
	"net/url"
	"strings"
)

// alwaysSafe is the set urllib.parse.quote never encodes, regardless of the
// caller's safe characters.
const alwaysSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"

// Quote percent-encodes value the way urllib.parse.quote(value, safe=safe)
// does: UTF-8 bytes outside the always-safe set and safe are escaped.
func Quote(value, safe string) string {
	var out strings.Builder
	out.Grow(len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if strings.IndexByte(alwaysSafe, char) >= 0 || strings.IndexByte(safe, char) >= 0 {
			out.WriteByte(char)
			continue
		}
		const hex = "0123456789ABCDEF"
		out.WriteByte('%')
		out.WriteByte(hex[char>>4])
		out.WriteByte(hex[char&0x0f])
	}
	return out.String()
}

// Scheme returns the lowercased URL scheme using urlsplit's rule: characters
// before the first colon, which must start with a letter and otherwise contain
// only letters, digits, '+', '-', or '.'.
func Scheme(raw string) string {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 {
		return ""
	}
	candidate := raw[:colon]
	for index := 0; index < len(candidate); index++ {
		char := candidate[index]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9', char == '+', char == '-', char == '.':
			if index == 0 {
				return ""
			}
		default:
			return ""
		}
	}
	return strings.ToLower(candidate)
}

// Join resolves ref against base the way urllib.parse.urljoin does. An
// unparseable input yields ref unchanged, which the callers then reject on the
// scheme check rather than treating as trusted.
func Join(base, ref string) string {
	if base == "" {
		return ref
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

// Hostname returns the lowercased host without any port, or "" when the URL
// cannot be parsed.
func Hostname(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

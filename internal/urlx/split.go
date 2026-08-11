package urlx

import (
	"strconv"
	"strings"
)

// Parts is the result of Split: the five components of a URL, unnormalized
// except for the scheme.
type Parts struct {
	Scheme   string
	Netloc   string
	Path     string
	Query    string
	Fragment string
}

// c0ControlOrSpace is the set urlsplit strips from both ends of a URL.
const c0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r" +
	"\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

// Split decomposes a URL the way urllib.parse.urlsplit does.
//
// Go's net/url.Parse is not a substitute here: it normalizes, rejects inputs
// urlsplit accepts, and resolves components differently. Herald stores the
// output as a deduplication key, so the exact decomposition is part of the
// on-disk contract.
func Split(raw string) Parts {
	// Tab and newline are removed anywhere in the URL, then C0 and space are
	// stripped from the ends. Both rules come from the WHATWG URL spec.
	replacer := strings.NewReplacer("\t", "", "\r", "", "\n", "")
	raw = strings.Trim(replacer.Replace(raw), c0ControlOrSpace)

	var parts Parts
	if colon := strings.IndexByte(raw, ':'); colon > 0 && isASCIILetter(raw[0]) {
		if isSchemeChars(raw[:colon]) {
			parts.Scheme = strings.ToLower(raw[:colon])
			raw = raw[colon+1:]
		}
	}
	if strings.HasPrefix(raw, "//") {
		end := len(raw)
		for index := 2; index < len(raw); index++ {
			if strings.IndexByte("/?#", raw[index]) >= 0 {
				end = index
				break
			}
		}
		parts.Netloc, raw = raw[2:end], raw[end:]
	}
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		parts.Fragment = raw[hash+1:]
		raw = raw[:hash]
	}
	if question := strings.IndexByte(raw, '?'); question >= 0 {
		parts.Query = raw[question+1:]
		raw = raw[:question]
	}
	parts.Path = raw
	return parts
}

// Hostname returns the lowercased host with userinfo, port, and IPv6 brackets
// removed, matching SplitResult.hostname.
func (p Parts) Hostname() string {
	host := p.Netloc
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return strings.ToLower(host[1:end])
		}
	}
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	return strings.ToLower(host)
}

// Port returns the explicit port and whether one was present. The error case
// mirrors SplitResult.port raising ValueError for a malformed or out-of-range
// port, which callers surface as a rejected URL rather than a crash.
func (p Parts) Port() (port int, present bool, err error) {
	host := p.Netloc
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if bracket := strings.IndexByte(host, ']'); bracket >= 0 {
		host = host[bracket+1:]
	} else if strings.Count(host, ":") > 1 {
		// A bare IPv6 literal has no port component.
		return 0, false, nil
	}
	colon := strings.IndexByte(host, ':')
	if colon < 0 {
		return 0, false, nil
	}
	digits := host[colon+1:]
	if digits == "" {
		return 0, false, nil
	}
	value, convErr := strconv.Atoi(digits)
	if convErr != nil {
		return 0, false, errInvalidPort
	}
	if value < 0 || value > 65535 {
		return 0, false, errInvalidPort
	}
	return value, true, nil
}

// Username reports whether the netloc carries userinfo, which Herald rejects
// on any URL it will fetch.
func (p Parts) HasUserinfo() bool {
	return strings.LastIndexByte(p.Netloc, '@') >= 0
}

type portError struct{}

func (portError) Error() string { return "invalid port" }

var errInvalidPort = portError{}

// ErrInvalidPort is returned by Port for a malformed or out-of-range port.
var ErrInvalidPort error = errInvalidPort

// usesNetloc are the schemes for which an empty authority is still written as
// "//", matching urllib.parse.uses_netloc.
var usesNetloc = map[string]bool{
	"": true, "ftp": true, "http": true, "gopher": true, "nntp": true,
	"telnet": true, "imap": true, "wais": true, "file": true, "mms": true,
	"https": true, "shttp": true, "snews": true, "prospero": true, "rtsp": true,
	"rtsps": true, "rtspu": true, "rsync": true, "svn": true, "svn+ssh": true,
	"sftp": true, "nfs": true, "git": true, "git+ssh": true, "ws": true,
	"wss": true, "itms-services": true,
}

// Unsplit reassembles components the way urllib.parse.urlunsplit does.
//
// The empty-authority rule is subtle and load-bearing: for a scheme that uses
// an authority, "https" + "" + "/x" renders as "https:///x", not "https:/x".
func Unsplit(p Parts) string {
	path := p.Path
	// hasNetloc distinguishes "no authority" from "empty authority".
	hasNetloc := p.Netloc != ""
	if !hasNetloc && usesNetloc[p.Scheme] && (path == "" || strings.HasPrefix(path, "/")) {
		hasNetloc = true
	}

	var out strings.Builder
	if hasNetloc {
		if path != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		path = "//" + p.Netloc + path
	} else if strings.HasPrefix(path, "//") {
		path = "//" + path
	}
	if p.Scheme != "" {
		out.WriteString(p.Scheme)
		out.WriteByte(':')
	}
	out.WriteString(path)
	if p.Query != "" {
		out.WriteByte('?')
		out.WriteString(p.Query)
	}
	if p.Fragment != "" {
		out.WriteByte('#')
		out.WriteString(p.Fragment)
	}
	return out.String()
}

func isASCIILetter(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func isSchemeChars(value string) bool {
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case isASCIILetter(char):
		case char >= '0' && char <= '9', char == '+', char == '-', char == '.':
		default:
			return false
		}
	}
	return true
}

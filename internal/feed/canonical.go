package feed

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// trackingParameters are removed from article URLs. The list is deliberately
// an explicit allowlist of known-tracking keys rather than a heuristic: a feed
// URL's query string frequently carries the article identity itself, so
// dropping unrecognized parameters would merge distinct articles.
var trackingParameters = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true,
	"mc_cid": true, "mc_eid": true, "vero_conv": true, "vero_id": true,
	"oly_anon_id": true, "oly_enc_id": true,
}

// CanonicalizeURL normalizes an article URL and removes only recognized
// tracking parameters.
//
// The result is stored as entries.canonical_url and used to deduplicate the
// same article across feeds, so this function's output is part of Herald's
// on-disk data and must stay stable.
//
// A malformed port is reported as an error rather than silently dropped: it
// makes the containing source refresh fail loudly, which is the established
// behaviour and the safer one for a URL Herald would otherwise fetch.
func CanonicalizeURL(value string) (string, error) {
	value = textx.Strip(value)
	if value == "" {
		return "", nil
	}
	parts := urlx.Split(value)
	scheme := strings.ToLower(parts.Scheme)
	if scheme != "http" && scheme != "https" {
		return value, nil
	}
	hostname := parts.Hostname()
	port, present, err := parts.Port()
	if err != nil {
		return "", fmt.Errorf("canonicalize %q: %w", value, err)
	}
	// Port 0 is falsy in the original implementation and so is dropped here
	// too, rather than being written back as an explicit ":0".
	if present && port != 0 && !isDefaultPort(scheme, port) {
		hostname = hostname + ":" + strconv.Itoa(port)
	}

	kept := make([]urlx.Pair, 0, 8)
	for _, pair := range urlx.ParseQuery(parts.Query) {
		folded := strings.ToLower(pair.Key)
		if strings.HasPrefix(folded, "utm_") || trackingParameters[folded] {
			continue
		}
		kept = append(kept, pair)
	}
	urlx.SortPairs(kept)

	path := parts.Path
	if path == "" {
		path = "/"
	}
	return urlx.Unsplit(urlx.Parts{
		Scheme: scheme,
		Netloc: hostname,
		Path:   path,
		Query:  urlx.EncodeQuery(kept),
	}), nil
}

func isDefaultPort(scheme string, port int) bool {
	return scheme == "http" && port == 80 || scheme == "https" && port == 443
}

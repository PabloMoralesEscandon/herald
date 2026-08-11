package markdown

import (
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// quoteSafe matches the safe set the exporter has always used, so already
// percent-encoded feed URLs are not double-escaped.
const quoteSafe = "/:#?[]@!$&'()*+,;=%"

var allowedSchemes = map[string]bool{"http": true, "https": true, "mailto": true}

// safeURL resolves a feed-supplied URL and rejects anything that is not a
// plain web or mail link. Relative references are kept only in the forms a
// document can legitimately use.
func safeURL(value, baseURL string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	resolved := value
	if baseURL != "" {
		resolved = urlx.Join(baseURL, value)
	}
	scheme := urlx.Scheme(resolved)
	if scheme != "" && !allowedSchemes[scheme] {
		return ""
	}
	if scheme == "" && !hasAnyPrefix(resolved, "/", "#", "./", "../") {
		return ""
	}
	return urlx.Quote(resolved, quoteSafe)
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

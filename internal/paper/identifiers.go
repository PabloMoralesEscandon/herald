package paper

import (
	"regexp"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

var (
	doiPattern   = regexp.MustCompile(`(?i)10\.\d{4,9}/[-._;()/:a-z0-9]+`)
	arxivPattern = regexp.MustCompile(
		`(?i)^(?:arxiv\s*:\s*)?((?:\d{4}\.\d{4,5}|[a-z-]+(?:\.[A-Z]{2})?/\d{7})(?:v\d+)?)$`)
	s2Pattern           = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)
	doiPrefixPattern    = regexp.MustCompile(`(?i)^(?:doi\s*:\s*|https?://(?:dx\.)?doi\.org/)`)
	arxivPrefixPattern  = regexp.MustCompile(`(?i)^https?://(?:www\.)?arxiv\.org/(?:abs|pdf)/`)
	arxivSuffixPattern  = regexp.MustCompile(`(?i)\.pdf$`)
	arxivVersionPattern = regexp.MustCompile(`(?i)v\d+$`)
)

// NormalizeDOI validates and lowercases a DOI, rejecting anything with
// trailing content so a URL is not silently accepted as a bare identifier.
func NormalizeDOI(value string) (string, error) {
	decoded := urlx.Unquote(strings.TrimSpace(value))
	decoded = doiPrefixPattern.ReplaceAllString(decoded, "")
	match := doiPattern.FindStringIndex(decoded)
	if match == nil {
		return "", &InvalidInputError{Reason: "Invalid DOI"}
	}
	doi := strings.ToLower(strings.TrimRight(decoded[match[0]:match[1]], ".,;"))
	if match[0] != 0 || strings.Trim(decoded[match[1]:], " .,)];}") != "" {
		return "", &InvalidInputError{Reason: "Invalid DOI"}
	}
	return doi, nil
}

// NormalizeArxivID validates an arXiv identifier and strips any version
// suffix, so v1 and v2 of a paper resolve to the same entry.
func NormalizeArxivID(value string) (string, error) {
	decoded := urlx.Unquote(strings.TrimSpace(value))
	decoded = arxivPrefixPattern.ReplaceAllString(decoded, "")
	decoded = arxivSuffixPattern.ReplaceAllString(decoded, "")
	match := arxivPattern.FindStringSubmatch(strings.TrimSpace(decoded))
	if match == nil {
		return "", &InvalidInputError{Reason: "Invalid arXiv identifier"}
	}
	return strings.ToLower(arxivVersionPattern.ReplaceAllString(match[1], "")), nil
}

// Locator is a resolved paper identity: a scheme plus its value, and the page
// URL when the input was a generic publication page.
type Locator struct {
	Scheme  string
	Value   string
	PageURL string
}

// ParseLocator resolves user input to a DOI, arXiv ID, Semantic Scholar ID, or
// a validated public paper page.
func ParseLocator(value string) (Locator, error) {
	raw := strings.TrimSpace(value)
	if raw == "" || len([]rune(raw)) > 2048 {
		return Locator{}, &InvalidInputError{Reason: "Paper identifier or URL is required"}
	}
	if doi, err := NormalizeDOI(raw); err == nil {
		return Locator{Scheme: "doi", Value: doi}, nil
	}
	if arxiv, err := NormalizeArxivID(raw); err == nil {
		return Locator{Scheme: "arxiv", Value: arxiv}, nil
	}

	parts := urlx.Split(raw)
	hostname := parts.Hostname()
	if strings.ToLower(parts.Scheme) != "https" || hostname == "" {
		return Locator{}, &InvalidInputError{
			Reason: "Use a DOI, arXiv ID, Semantic Scholar URL, or public HTTPS paper URL"}
	}
	hostname = strings.TrimPrefix(hostname, "www.")
	switch {
	case hostname == "doi.org":
		doi, err := NormalizeDOI(raw)
		if err != nil {
			return Locator{}, err
		}
		return Locator{Scheme: "doi", Value: doi}, nil
	case hostname == "arxiv.org":
		arxiv, err := NormalizeArxivID(raw)
		if err != nil {
			return Locator{}, err
		}
		return Locator{Scheme: "arxiv", Value: arxiv}, nil
	case strings.HasSuffix(hostname, "semanticscholar.org"):
		var candidate string
		for _, segment := range strings.Split(parts.Path, "/") {
			if segment != "" {
				candidate = segment
			}
		}
		if !s2Pattern.MatchString(candidate) {
			return Locator{}, &InvalidInputError{
				Reason: "Semantic Scholar URL does not contain a paper ID"}
		}
		return Locator{Scheme: "s2", Value: strings.ToLower(candidate)}, nil
	}
	// A generic page is accepted only after passing the public-HTTPS checks.
	// DNS is not resolved here: this runs on the request path, and the fetch
	// itself performs the resolving check.
	safeURL, err := validatePublicHTTPSURL(raw, false)
	if err != nil {
		return Locator{}, err
	}
	return Locator{Scheme: "url", Value: safeURL, PageURL: safeURL}, nil
}

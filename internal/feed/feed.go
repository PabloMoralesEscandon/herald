// Package feed parses RSS 2.0, RSS 1.0/RDF, and Atom documents into the single
// normalized entry shape Herald stores, and canonicalizes the URLs it finds.
package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/markdown"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// ParseError reports a document that is not a supported or well-formed feed.
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return e.Reason }

// IsParseError reports whether err came from feed parsing, which callers treat
// as a source-health problem rather than a bug.
func IsParseError(err error) bool {
	var parseError *ParseError
	return errors.As(err, &parseError)
}

// Entry is one normalized feed item.
//
// Content is carried twice on purpose: Content is plain text for Herald's
// reader and summarizer, ContentMarkdown preserves structure for the Obsidian
// export.
type Entry struct {
	GUID            string
	URL             string
	Title           string
	Author          string
	PublishedAt     string
	Content         string
	ContentMarkdown string
}

// Parse converts a feed document into normalized entries.
func Parse(document []byte) ([]Entry, error) {
	root, err := parseXML(document)
	if err != nil {
		return nil, &ParseError{Reason: err.Error()}
	}
	switch root.name {
	case "feed":
		return parseAtom(root)
	case "rss", "rdf":
		return parseRSS(root)
	}
	return nil, &ParseError{Reason: fmt.Sprintf("unsupported feed root element: %s", root.name)}
}

func parseAtom(root *element) ([]Entry, error) {
	items := root.childrenNamed("entry")
	parsed := make([]Entry, 0, len(items))
	for _, item := range items {
		title := plainText(elementText(item.child("title")))
		if title == "" {
			title = "Untitled"
		}
		url, err := CanonicalizeURL(atomLink(item))
		if err != nil {
			return nil, err
		}
		published := publishedAt(elementText(item.child("published", "updated")))
		guid := elementText(item.child("id"))
		if guid == "" {
			guid = url
		}
		markup := elementMarkup(item.child("content", "summary"))
		if guid == "" {
			guid = generatedGUID(title, url, published)
		}
		parsed = append(parsed, Entry{
			GUID:            guid,
			URL:             url,
			Title:           title,
			Author:          atomAuthor(item),
			PublishedAt:     published,
			Content:         plainText(markup),
			ContentMarkdown: markdown.ToMarkdown(markup, url),
		})
	}
	return parsed, nil
}

func parseRSS(root *element) ([]Entry, error) {
	container := root
	// RSS 1.0/RDF places items beside the channel rather than inside it.
	if channel := root.child("channel"); channel != nil && root.name != "rdf" {
		container = channel
	}
	items := container.childrenNamed("item")
	parsed := make([]Entry, 0, len(items))
	for _, item := range items {
		title := plainText(elementText(item.child("title")))
		if title == "" {
			title = "Untitled"
		}
		url, err := CanonicalizeURL(rssLink(item))
		if err != nil {
			return nil, err
		}
		published := publishedAt(elementText(item.child("pubdate", "published", "date", "updated")))
		guid := elementText(item.child("guid", "id"))
		if guid == "" {
			guid = url
		}
		markup := elementMarkup(item.child("encoded", "content", "description", "summary"))
		if guid == "" {
			guid = generatedGUID(title, url, published)
		}
		parsed = append(parsed, Entry{
			GUID:            guid,
			URL:             url,
			Title:           title,
			Author:          plainText(elementText(item.child("creator", "author"))),
			PublishedAt:     published,
			Content:         plainText(markup),
			ContentMarkdown: markdown.ToMarkdown(markup, url),
		})
	}
	return parsed, nil
}

// atomLink prefers rel="alternate", which is the canonical article link, and
// falls back to the first link with an href.
func atomLink(item *element) string {
	fallback := ""
	for _, link := range item.childrenNamed("link") {
		href := strings.TrimSpace(link.attr["href"])
		if href == "" {
			continue
		}
		relation := strings.ToLower(link.attr["rel"])
		if relation == "" {
			relation = "alternate"
		}
		if relation == "alternate" {
			return href
		}
		if fallback == "" {
			fallback = href
		}
	}
	return fallback
}

func atomAuthor(item *element) string {
	var authors []string
	for _, author := range item.childrenNamed("author") {
		name := elementText(author.child("name"))
		if name == "" {
			name = elementText(author)
		}
		if name != "" {
			authors = append(authors, name)
		}
	}
	return strings.Join(authors, ", ")
}

func rssLink(item *element) string {
	link := item.child("link")
	if link == nil {
		return ""
	}
	if href := strings.TrimSpace(link.attr["href"]); href != "" {
		return href
	}
	return elementText(link)
}

// publishedAt normalizes a feed timestamp to UTC ISO 8601, accepting both the
// RFC 2822 form RSS uses and the ISO form Atom uses. An unrecognized value is
// preserved verbatim rather than discarded.
func publishedAt(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := mail.ParseDate(value); err == nil {
		return parsed.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05-07:00")
	}
	if parsed, ok := parseISO(value); ok {
		return parsed.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05-07:00")
	}
	return value
}

// isoLayouts covers the forms datetime.fromisoformat accepts that appear in
// feeds, including a bare date and a space-separated datetime.
var isoLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseISO(value string) (time.Time, bool) {
	candidate := strings.Replace(value, "Z", "+00:00", 1)
	for _, layout := range isoLayouts {
		if parsed, err := time.Parse(layout, candidate); err == nil {
			// A naive timestamp is treated as UTC, matching the original.
			return parsed, true
		}
	}
	return time.Time{}, false
}

// generatedGUID is a stable identity for feeds that supply no id at all.
func generatedGUID(title, url, published string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{title, url, published}, "\x1f")))
	return "herald:" + hex.EncodeToString(digest[:])
}

// DiscoverFeedURL returns the first RSS/Atom autodiscovery link in an HTML
// document.
//
// This deliberately reads only standard <link rel=alternate> metadata; Herald
// does not scrape article or index pages.
func DiscoverFeedURL(document []byte, pageURL string) string {
	links := autodiscoveryLinks(string(document))
	if len(links) == 0 {
		return ""
	}
	discovered := urlx.Join(pageURL, links[0])
	parts := urlx.Split(discovered)
	scheme := strings.ToLower(parts.Scheme)
	if scheme != "http" && scheme != "https" || parts.Netloc == "" {
		return ""
	}
	canonical, err := CanonicalizeURL(discovered)
	if err != nil {
		return ""
	}
	return canonical
}

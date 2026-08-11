package feed

import (
	"strings"

	"golang.org/x/net/html"
)

// feedMediaTypes are the only types Herald accepts from an autodiscovery link.
var feedMediaTypes = map[string]bool{
	"application/rss+xml":  true,
	"application/atom+xml": true,
	"application/rdf+xml":  true,
}

// autodiscoveryLinks extracts hrefs from <link rel="alternate" type="...feed">
// elements, in document order.
func autodiscoveryLinks(document string) []string {
	var links []string
	tokenizer := html.NewTokenizer(strings.NewReader(document))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return links
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if !strings.EqualFold(token.Data, "link") {
				continue
			}
			var relations, mediaType, href string
			for _, attribute := range token.Attr {
				switch strings.ToLower(attribute.Key) {
				case "rel":
					relations = attribute.Val
				case "type":
					mediaType = attribute.Val
				case "href":
					href = attribute.Val
				}
			}
			if !hasRelation(relations, "alternate") {
				continue
			}
			mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
			if !feedMediaTypes[mediaType] {
				continue
			}
			if href = strings.TrimSpace(href); href != "" {
				links = append(links, href)
			}
		}
	}
}

// hasRelation reports whether the space-separated rel attribute contains the
// wanted relation, case-insensitively.
func hasRelation(relations, wanted string) bool {
	for _, relation := range strings.Fields(relations) {
		if strings.EqualFold(relation, wanted) {
			return true
		}
	}
	return false
}

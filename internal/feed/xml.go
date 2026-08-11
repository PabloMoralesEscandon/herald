package feed

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// element is a minimal, namespace-insensitive XML tree.
//
// Feeds in the wild mix RSS, Atom, Dublin Core, and content-module elements
// with inconsistent prefixes, so every lookup here matches on local name only.
//
// text and tail follow the ElementTree model: text is the character data
// before the first child, tail is the data after this element's closing tag.
// Splitting them is what preserves document order in mixed content such as
// "Read <a>the link</a>." — collapsing both into one field reorders the
// sentence.
type element struct {
	name     string
	attr     map[string]string
	text     string
	tail     string
	children []*element
}

// parseXML builds the tree with encoding/xml's tokenizer.
func parseXML(document []byte) (*element, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(document)))
	decoder.Strict = true
	decoder.CharsetReader = charsetReader
	// Entity is deliberately left at the XML defaults. A feed using an HTML
	// entity such as &nbsp; is malformed XML and is rejected outright, which
	// keeps ingestion identical to the reference implementation instead of
	// silently accepting documents it would have refused.

	var root *element
	var stack []*element
	// lastClosed tracks, per open element, the child that most recently
	// closed, so trailing character data is attributed to its tail.
	var lastClosed []*element

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid XML feed: %w", err)
		}
		switch typed := token.(type) {
		case xml.StartElement:
			node := &element{
				name: strings.ToLower(typed.Name.Local),
				attr: make(map[string]string, len(typed.Attr)),
			}
			for _, attribute := range typed.Attr {
				node.attr[strings.ToLower(attribute.Name.Local)] = attribute.Value
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			} else if root == nil {
				root = node
			}
			stack = append(stack, node)
			lastClosed = append(lastClosed, nil)
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			closed := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			lastClosed = lastClosed[:len(lastClosed)-1]
			if len(lastClosed) > 0 {
				lastClosed[len(lastClosed)-1] = closed
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			if previous := lastClosed[len(lastClosed)-1]; previous != nil {
				previous.tail += string(typed)
			} else {
				stack[len(stack)-1].text += string(typed)
			}
		}
	}
	if root == nil {
		return nil, errors.New("invalid XML feed: no root element")
	}
	return root, nil
}

// child returns the first child matching any of the local names, in the order
// the names are given.
func (e *element) child(names ...string) *element {
	for _, name := range names {
		for _, candidate := range e.children {
			if candidate.name == name {
				return candidate
			}
		}
	}
	return nil
}

// childrenNamed returns every direct child with the given local name.
func (e *element) childrenNamed(name string) []*element {
	var matches []*element
	for _, candidate := range e.children {
		if candidate.name == name {
			matches = append(matches, candidate)
		}
	}
	return matches
}

// itertext concatenates character data in document order, matching
// ElementTree's itertext(). The element's own tail is excluded, as there.
func (e *element) itertext() string {
	if e == nil {
		return ""
	}
	var out strings.Builder
	var walk func(node *element)
	walk = func(node *element) {
		out.WriteString(node.text)
		for _, child := range node.children {
			walk(child)
			out.WriteString(child.tail)
		}
	}
	walk(e)
	return out.String()
}

// elementText is the trimmed text of an element, or "" when absent.
func elementText(e *element) string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.itertext())
}

// elementMarkup preserves embedded feed markup, including Atom's namespaced
// XHTML form, by re-serializing child elements without their namespaces.
//
// This is what lets rich Atom content survive into Obsidian: taking only the
// text would discard every link, list, and heading. An element with no child
// elements is returned verbatim, which is how CDATA-wrapped HTML reaches the
// Markdown converter unescaped.
func elementMarkup(e *element) string {
	if e == nil {
		return ""
	}
	if len(e.children) == 0 {
		return strings.TrimSpace(e.text)
	}
	var out strings.Builder
	out.WriteString(escapeText(e.text))
	for _, child := range e.children {
		serializeElement(&out, child)
		out.WriteString(escapeText(child.tail))
	}
	return out.String()
}

func serializeElement(out *strings.Builder, node *element) {
	out.WriteByte('<')
	out.WriteString(node.name)
	// The attribute map loses source order, so emit sorted for determinism.
	// Feed markup does not depend on attribute order.
	for _, key := range sortedKeys(node.attr) {
		out.WriteByte(' ')
		out.WriteString(key)
		out.WriteString(`="`)
		out.WriteString(escapeAttribute(node.attr[key]))
		out.WriteByte('"')
	}
	out.WriteByte('>')
	out.WriteString(escapeText(node.text))
	for _, child := range node.children {
		serializeElement(out, child)
		out.WriteString(escapeText(child.tail))
	}
	out.WriteString("</")
	out.WriteString(node.name)
	out.WriteByte('>')
}

// escapeText matches html.escape(value, quote=False).
//
// Go's html.EscapeString is not equivalent: it also escapes quotes, and spells
// the entities differently, which would change stored content byte-for-byte.
var escapeText = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
).Replace

// escapeAttribute matches html.escape(value, quote=True), including Python's
// choice of &#x27; for the apostrophe.
var escapeAttribute = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#x27;",
).Replace

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

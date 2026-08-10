from __future__ import annotations

import re
from dataclasses import dataclass, field
from html.parser import HTMLParser
from urllib.parse import quote, urljoin, urlsplit


_HTML_TAG = re.compile(
    r"</?(?:a|article|aside|b|blockquote|br|code|dd|del|details|div|dl|dt|em|"
    r"embed|figcaption|figure|footer|h[1-6]|header|hr|i|iframe|img|kbd|li|main|mark|nav|object|ol|"
    r"p|pre|q|s|script|section|small|span|strike|strong|style|sub|summary|sup|svg|"
    r"table|tbody|td|template|tfoot|th|thead|time|tr|u|ul)\b",
    re.IGNORECASE,
)
_BLOCK_TAGS = {
    "article",
    "aside",
    "dd",
    "details",
    "div",
    "dl",
    "dt",
    "figcaption",
    "figure",
    "footer",
    "header",
    "main",
    "nav",
    "p",
    "section",
    "summary",
}
_IGNORED_TAGS = {"embed", "iframe", "object", "script", "style", "svg", "template"}


@dataclass(slots=True)
class _Node:
    tag: str
    attrs: dict[str, str] = field(default_factory=dict)
    children: list[_Node | str] = field(default_factory=list)


class _TreeBuilder(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.root = _Node("document")
        self.stack = [self.root]

    def _close_open(self, tags: set[str], boundaries: set[str]) -> None:
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag in boundaries:
                return
            if self.stack[index].tag in tags:
                del self.stack[index:]
                return

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        name = tag.casefold()
        if name == "li":
            self._close_open({"li"}, {"ol", "ul"})
        elif name == "p":
            self._close_open({"p"}, _BLOCK_TAGS - {"p"})
        elif name == "tr":
            self._close_open({"tr"}, {"table", "tbody", "tfoot", "thead"})
        elif name in {"td", "th"}:
            self._close_open({"td", "th"}, {"tr"})
        node = _Node(
            name,
            {key.casefold(): value or "" for key, value in attrs},
        )
        self.stack[-1].children.append(node)
        if name not in {"br", "hr", "img", "input", "meta", "link"}:
            self.stack.append(node)

    def handle_startendtag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        self.handle_starttag(tag, attrs)
        if self.stack[-1].tag == tag.casefold():
            self.stack.pop()

    def handle_endtag(self, tag: str) -> None:
        name = tag.casefold()
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag == name:
                del self.stack[index:]
                return

    def handle_data(self, data: str) -> None:
        if data:
            self.stack[-1].children.append(data)


def _text(node: _Node) -> str:
    return "".join(
        child if isinstance(child, str) else _text(child)
        for child in node.children
    )


def _fallback_text(root: _Node) -> str:
    """Extract safe text iteratively if pathological nesting defeats rendering."""
    parts: list[str] = []
    pending: list[_Node | str] = [root]
    while pending:
        current = pending.pop()
        if isinstance(current, str):
            parts.append(current)
            continue
        if current.tag in _IGNORED_TAGS:
            continue
        pending.extend(reversed(current.children))
        if current.tag in _BLOCK_TAGS or current.tag in {"br", "li"}:
            parts.append("\n")
    return re.sub(r"\s+", " ", "".join(parts)).strip()


def _safe_url(value: str, base_url: str) -> str:
    value = value.strip()
    if not value:
        return ""
    resolved = urljoin(base_url, value) if base_url else value
    scheme = urlsplit(resolved).scheme.casefold()
    if scheme and scheme not in {"http", "https", "mailto"}:
        return ""
    if not scheme and not resolved.startswith(("/", "#", "./", "../")):
        return ""
    return quote(resolved, safe="/:#?[]@!$&'()*+,;=%")


def _label(value: str, fallback: str = "link") -> str:
    value = re.sub(r"\s+", " ", value).strip() or fallback
    return value.replace("[", r"\[").replace("]", r"\]")


def _inline_code(value: str) -> str:
    value = re.sub(r"\s+", " ", value).strip()
    if not value:
        return ""
    fence = "``" if "`" in value else "`"
    padding = " " if value.startswith("`") or value.endswith("`") else ""
    return f"{fence}{padding}{value}{padding}{fence}"


def _render_table(node: _Node, base_url: str) -> str:
    rows: list[tuple[list[str], bool]] = []

    def visit(current: _Node) -> None:
        if current.tag == "tr":
            cells = [
                re.sub(r"\s+", " ", _render(child, base_url).strip())
                for child in current.children
                if isinstance(child, _Node) and child.tag in {"td", "th"}
            ]
            if cells:
                header = any(
                    isinstance(child, _Node) and child.tag == "th"
                    for child in current.children
                )
                rows.append((cells, header))
            return
        for child in current.children:
            if isinstance(child, _Node):
                visit(child)

    visit(node)
    if not rows:
        return ""
    width = max(len(cells) for cells, _ in rows)
    normalized = [
        ([*cells, *([""] * (width - len(cells)))], header)
        for cells, header in rows
    ]
    header_index = next(
        (index for index, (_, is_header) in enumerate(normalized) if is_header),
        0,
    )
    header = normalized.pop(header_index)[0]

    def row(cells: list[str]) -> str:
        escaped = [cell.replace("|", r"\|") for cell in cells]
        return "| " + " | ".join(escaped) + " |"

    return "\n".join(
        [row(header), row(["---"] * width), *(row(cells) for cells, _ in normalized)]
    )


def _render_list(node: _Node, base_url: str, depth: int = 0) -> str:
    lines: list[str] = []
    number = 1
    for child in node.children:
        if not isinstance(child, _Node) or child.tag != "li":
            continue
        nested = [
            item
            for item in child.children
            if isinstance(item, _Node) and item.tag in {"ul", "ol"}
        ]
        body = "".join(
            item if isinstance(item, str) else _render(item, base_url)
            for item in child.children
            if not (isinstance(item, _Node) and item.tag in {"ul", "ol"})
        )
        body = re.sub(r"\s*\n\s*", " ", body).strip()
        marker = f"{number}." if node.tag == "ol" else "-"
        indentation = "  " * depth
        lines.append(f"{indentation}{marker} {body}".rstrip())
        for nested_list in nested:
            lines.append(_render_list(nested_list, base_url, depth + 1))
        number += 1
    return "\n".join(line for line in lines if line)


def _render(node: _Node, base_url: str) -> str:
    tag = node.tag
    if tag in _IGNORED_TAGS:
        return ""
    if tag == "pre":
        value = _text(node).strip("\n")
        if not value:
            return ""
        fence = "````" if "```" in value else "```"
        language = ""
        first = next((child for child in node.children if isinstance(child, _Node)), None)
        if first is not None and first.tag == "code":
            classes = first.attrs.get("class", "").split()
            language = next(
                (
                    item.removeprefix("language-")
                    for item in classes
                    if item.startswith("language-")
                ),
                "",
            )
        return f"\n\n{fence}{language}\n{value}\n{fence}\n\n"

    children = "".join(
        re.sub(r"\s+", " ", child) if isinstance(child, str) else _render(child, base_url)
        for child in node.children
    )
    if tag == "document":
        return children
    if tag in _BLOCK_TAGS:
        return f"\n\n{children.strip()}\n\n" if children.strip() else ""
    if tag in {"h1", "h2", "h3", "h4", "h5", "h6"}:
        return f"\n\n{'#' * int(tag[1])} {children.strip()}\n\n"
    if tag in {"strong", "b"}:
        return f"**{children.strip()}**" if children.strip() else ""
    if tag in {"em", "i"}:
        return f"*{children.strip()}*" if children.strip() else ""
    if tag in {"del", "s", "strike"}:
        return f"~~{children.strip()}~~" if children.strip() else ""
    if tag == "mark":
        return f"=={children.strip()}==" if children.strip() else ""
    if tag == "code":
        return _inline_code(_text(node))
    if tag == "br":
        return "\n"
    if tag == "hr":
        return "\n\n---\n\n"
    if tag == "a":
        href = _safe_url(node.attrs.get("href", ""), base_url)
        label = _label(children, href or "link")
        return f"[{label}](<{href}>)" if href else label
    if tag == "img":
        source = _safe_url(node.attrs.get("src", ""), base_url)
        if not source:
            return ""
        return f"![{_label(node.attrs.get('alt', ''), 'image')}](<{source}>)"
    if tag in {"ul", "ol"}:
        return f"\n\n{_render_list(node, base_url)}\n\n"
    if tag == "li":
        return children
    if tag == "blockquote":
        value = children.strip()
        return "\n\n" + "\n".join(
            f"> {line}" if line else ">" for line in value.splitlines()
        ) + "\n\n"
    if tag == "table":
        table = _render_table(node, base_url)
        return f"\n\n{table}\n\n" if table else ""
    return children


def html_to_markdown(value: str, *, base_url: str = "") -> str:
    """Convert feed HTML to conservative Obsidian-compatible Markdown.

    Plain text and existing Markdown pass through unchanged. Unsupported tags
    lose only their markup, while executable and styling elements are removed
    with their contents.
    """
    if not value:
        return ""
    value = value.replace("\r\n", "\n").replace("\r", "\n")
    if _HTML_TAG.search(value) is None:
        return value.strip()
    parser = _TreeBuilder()
    try:
        parser.feed(value)
        parser.close()
    except (ValueError, RecursionError):
        return _fallback_text(parser.root)
    try:
        markdown = _render(parser.root, base_url)
    except RecursionError:
        return _fallback_text(parser.root)
    markdown = re.sub(r"[ \t]+\n", "\n", markdown)
    markdown = re.sub(r"\n{3,}", "\n\n", markdown)
    return markdown.strip()

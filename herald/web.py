from __future__ import annotations

import json
import mimetypes
from html import escape
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from .config import Settings
from .obsidian import ObsidianExporter
from .service import HeraldService
from .storage import Database
from .summaries import LocalSummarizer


STATIC_DIR = Path(__file__).with_name("static")
STATIC_FILES = {
    "/": "index.html",
    "/index.html": "index.html",
    "/static/styles.css": "styles.css",
    "/static/app.js": "app.js",
}
ACTION_STATUSES = {
    "read": "read",
    "unread": "unread",
    "keep": "kept",
    "discard": "discarded",
}


class HeraldServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(
        self,
        server_address: tuple[str, int],
        database: Database,
        settings: Settings,
        service: HeraldService,
    ) -> None:
        super().__init__(server_address, HeraldRequestHandler)
        self.database = database
        self.settings = settings
        self.service = service


class HeraldRequestHandler(BaseHTTPRequestHandler):
    server: HeraldServer

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlparse(self.path)
        if parsed.path in STATIC_FILES:
            self._serve_static(parsed.path)
            return
        if parsed.path == "/api/entries":
            self._list_entries(parse_qs(parsed.query))
            return
        if parsed.path == "/api/sources":
            self._send_json(self.server.database.list_sources())
            return
        if parsed.path == "/api/stats":
            self._send_json(self.server.database.entry_counts())
            return
        page_entry_id = self._page_entry_id(parsed.path)
        if page_entry_id is not None:
            self._serve_entry_page(page_entry_id)
            return
        entry_id = self._entry_id(parsed.path)
        if entry_id is not None:
            self._get_entry(entry_id)
            return
        self._send_error(HTTPStatus.NOT_FOUND, "Not found")

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        path = urlparse(self.path).path
        page_action = self._entry_page_subroute(path)
        if page_action is not None:
            entry_id, action = page_action
            self._handle_entry_page_action(entry_id, action)
            return
        if path == "/api/sources":
            self._add_source()
            return
        if path == "/api/refresh":
            self._refresh()
            return
        entry_action = self._entry_subroute(path, "action")
        if entry_action is not None:
            self._change_status(entry_action)
            return
        entry_summary = self._entry_subroute(path, "summarize")
        if entry_summary is not None:
            self._summarize(entry_summary)
            return
        entry_export = self._entry_subroute(path, "export")
        if entry_export is not None:
            self._export(entry_export)
            return
        self._send_error(HTTPStatus.NOT_FOUND, "Not found")

    def _serve_static(self, request_path: str) -> None:
        file_path = STATIC_DIR / STATIC_FILES[request_path]
        try:
            content = file_path.read_bytes()
        except FileNotFoundError:
            self._send_error(HTTPStatus.NOT_FOUND, "Web asset not found")
            return
        content_type = mimetypes.guess_type(file_path.name)[0] or "application/octet-stream"
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", f"{content_type}; charset=utf-8")
        self.send_header("Content-Length", str(len(content)))
        self.send_header("Cache-Control", "no-store, max-age=0")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(content)

    def _serve_entry_page(
        self,
        entry_id: int,
        *,
        message: str = "",
        error: str = "",
        status: HTTPStatus = HTTPStatus.OK,
    ) -> None:
        entry = self.server.database.get_entry(entry_id)
        if entry is None:
            self._send_html(
                "<!doctype html><title>Not found — Herald</title>"
                '<main class="article-page"><h1>Article not found</h1>'
                '<p><a href="/">Return to the inbox</a></p></main>',
                HTTPStatus.NOT_FOUND,
            )
            return
        kept = entry["status"] == "kept"
        read_action = "unread" if entry["status"] == "read" else "read"
        read_label = "Mark unread" if read_action == "unread" else "Mark read"
        summary = entry["summary"] or "No summary has been generated yet."
        exported = (
            f'<p class="page-notice success">Exported to {escape(entry["exported_path"])}</p>'
            if entry["exported_path"]
            else ""
        )
        notice = (
            f'<p class="page-notice success">{escape(message)}</p>' if message else ""
        )
        problem = f'<p class="page-notice error">{escape(error)}</p>' if error else ""
        disabled = "" if kept else " disabled"
        disabled_hint = "" if kept else "<small>Keep this article before exporting.</small>"
        document = f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{escape(entry['title'])} — Herald</title>
  <link rel="stylesheet" href="/static/styles.css">
</head>
<body class="article-page-body">
  <main class="article-page">
    <a class="page-back" href="/">← Back to inbox</a>
    {notice}{problem}{exported}
    <div class="reader-source"><span>{escape(entry['source_category'])}</span><span>·</span><span>{escape(entry['source_title'])}</span></div>
    <h1>{escape(entry['title'])}</h1>
    <p class="page-byline">{escape(entry['author'] or 'Unknown author')} · {escape(entry['published_at'] or 'Date unavailable')}</p>
    <p class="status-chip {escape(entry['status'])}">{escape(entry['status'])}</p>
    <div class="page-actions">
      <form method="post" action="/entry/{entry_id}/action"><button name="action" value="{read_action}">{read_label}</button></form>
      <form method="post" action="/entry/{entry_id}/action"><button name="action" value="keep">Keep</button></form>
      <form method="post" action="/entry/{entry_id}/action"><button name="action" value="discard">Discard</button></form>
      <form method="post" action="/entry/{entry_id}/summarize"><button>Generate summary</button></form>
      <form method="post" action="/entry/{entry_id}/export"><button{disabled}>Export to Obsidian</button>{disabled_hint}</form>
    </div>
    <section class="summary-card">
      <div class="summary-heading"><div><span class="spark">✦</span><h2>Herald summary</h2></div></div>
      <p>{escape(summary)}</p>
    </section>
    <section class="page-excerpt"><h2>From the feed</h2><p>{escape(entry['content'] or 'The feed did not provide an excerpt.')}</p></section>
    <a class="open-link" href="{escape(entry['url'], quote=True)}" target="_blank" rel="noopener noreferrer">Read original ↗</a>
  </main>
</body>
</html>"""
        self._send_html(document, status)

    def _handle_entry_page_action(self, entry_id: int, action: str) -> None:
        entry = self.server.database.get_entry(entry_id)
        if entry is None:
            self._serve_entry_page(entry_id)
            return
        if action == "action":
            form = self._read_form()
            if form is None:
                return
            requested = form.get("action", [""])[0]
            status = ACTION_STATUSES.get(requested)
            if status is None:
                self._serve_entry_page(
                    entry_id, error="Unknown article action.", status=HTTPStatus.BAD_REQUEST
                )
                return
            self.server.database.set_status(entry_id, status)
            self._redirect_to_entry(entry_id)
            return
        if action == "summarize":
            try:
                self.server.service.summarize_entry(entry_id)
            except KeyError:
                self._serve_entry_page(entry_id)
                return
            self._redirect_to_entry(entry_id)
            return
        if action == "export":
            try:
                self.server.service.export_entry(entry_id)
            except ValueError as problem:
                self._serve_entry_page(
                    entry_id, error=str(problem), status=HTTPStatus.CONFLICT
                )
                return
            self._redirect_to_entry(entry_id)
            return
        self._send_error(HTTPStatus.NOT_FOUND, "Not found")

    def _read_form(self) -> dict[str, list[str]] | None:
        if "application/x-www-form-urlencoded" not in self.headers.get(
            "Content-Type", ""
        ):
            self._send_error(
                HTTPStatus.UNSUPPORTED_MEDIA_TYPE,
                "Content-Type must be application/x-www-form-urlencoded",
            )
            return None
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self._send_error(HTTPStatus.BAD_REQUEST, "Invalid Content-Length")
            return None
        return parse_qs(self.rfile.read(min(length, 100_000)).decode("utf-8"))

    def _redirect_to_entry(self, entry_id: int) -> None:
        self.send_response(HTTPStatus.SEE_OTHER)
        self.send_header("Location", f"/entry/{entry_id}")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()

    def _list_entries(self, query: dict[str, list[str]]) -> None:
        status = query.get("status", [None])[0] or None
        category = query.get("category", [None])[0] or None
        try:
            limit = int(query.get("limit", ["100"])[0])
            entries = self.server.database.list_entries(
                status=status,
                category=category,
                limit=limit,
            )
        except (TypeError, ValueError) as error:
            self._send_error(HTTPStatus.BAD_REQUEST, str(error))
            return
        self._send_json(entries)

    def _get_entry(self, entry_id: int) -> None:
        entry = self.server.database.get_entry(entry_id)
        if entry is None:
            self._send_error(HTTPStatus.NOT_FOUND, "Entry not found")
            return
        self._send_json(entry)

    def _add_source(self) -> None:
        payload = self._read_json()
        if payload is None:
            return
        title = self._required_text(payload, "title")
        url = self._required_text(payload, "url")
        category = self._required_text(payload, "category")
        if not all((title, url, category)):
            self._send_error(
                HTTPStatus.BAD_REQUEST,
                "title, url, and category are required",
            )
            return
        parsed_url = urlparse(url)
        if parsed_url.scheme not in {"http", "https"} or not parsed_url.netloc:
            self._send_error(HTTPStatus.BAD_REQUEST, "url must be an HTTP(S) URL")
            return
        source_id = self.server.database.add_source(title, url, category)
        source = next(
            source
            for source in self.server.database.list_sources()
            if source["id"] == source_id
        )
        self._send_json(source, HTTPStatus.CREATED)

    def _change_status(self, entry_id: int) -> None:
        payload = self._read_json()
        if payload is None:
            return
        action = self._required_text(payload, "action")
        status = ACTION_STATUSES.get(action)
        if status is None:
            self._send_error(
                HTTPStatus.BAD_REQUEST,
                "action must be read, unread, keep, or discard",
            )
            return
        if not self.server.database.set_status(entry_id, status):
            self._send_error(HTTPStatus.NOT_FOUND, "Entry not found")
            return
        self._send_json(self.server.database.get_entry(entry_id))

    def _refresh(self) -> None:
        results = self.server.service.refresh_all()
        payload = [result.to_dict() for result in results]
        self._send_json(
            {
                "sources": payload,
                "created": sum(result.created for result in results),
                "updated": sum(result.updated for result in results),
                "errors": sum(result.error is not None for result in results),
            }
        )

    def _summarize(self, entry_id: int) -> None:
        try:
            result = self.server.service.summarize_entry(entry_id)
        except KeyError:
            self._send_error(HTTPStatus.NOT_FOUND, "Entry not found")
            return
        entry = self.server.database.get_entry(entry_id)
        self._send_json(
            {
                "entry": entry,
                "provider": result.provider,
                "model": result.model,
                "fallback_reason": result.fallback_reason,
            }
        )

    def _export(self, entry_id: int) -> None:
        try:
            destination = self.server.service.export_entry(entry_id)
        except KeyError:
            self._send_error(HTTPStatus.NOT_FOUND, "Entry not found")
            return
        except ValueError as error:
            self._send_error(HTTPStatus.CONFLICT, str(error))
            return
        except OSError as error:
            self._send_error(HTTPStatus.INTERNAL_SERVER_ERROR, f"Export failed: {error}")
            return
        self._send_json(
            {
                "entry": self.server.database.get_entry(entry_id),
                "path": str(destination),
            }
        )

    def _read_json(self) -> dict[str, Any] | None:
        if "application/json" not in self.headers.get("Content-Type", ""):
            self._send_error(
                HTTPStatus.UNSUPPORTED_MEDIA_TYPE,
                "Content-Type must be application/json",
            )
            return None
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self._send_error(HTTPStatus.BAD_REQUEST, "Invalid Content-Length")
            return None
        if length > 1_000_000:
            self._send_error(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, "Request is too large")
            return None
        try:
            payload = json.loads(self.rfile.read(length))
        except (json.JSONDecodeError, UnicodeDecodeError):
            self._send_error(HTTPStatus.BAD_REQUEST, "Invalid JSON")
            return None
        if not isinstance(payload, dict):
            self._send_error(HTTPStatus.BAD_REQUEST, "JSON body must be an object")
            return None
        return payload

    @staticmethod
    def _required_text(payload: dict[str, Any], key: str) -> str:
        value = payload.get(key)
        return value.strip() if isinstance(value, str) else ""

    @staticmethod
    def _entry_id(path: str) -> int | None:
        parts = path.strip("/").split("/")
        if len(parts) != 3 or parts[:2] != ["api", "entries"]:
            return None
        try:
            return int(unquote(parts[2]))
        except ValueError:
            return None

    @staticmethod
    def _entry_subroute(path: str, subroute: str) -> int | None:
        parts = path.strip("/").split("/")
        if len(parts) != 4 or parts[:2] != ["api", "entries"]:
            return None
        if parts[3] != subroute:
            return None
        try:
            return int(unquote(parts[2]))
        except ValueError:
            return None

    @staticmethod
    def _page_entry_id(path: str) -> int | None:
        parts = path.strip("/").split("/")
        if len(parts) != 2 or parts[0] != "entry":
            return None
        try:
            return int(parts[1])
        except ValueError:
            return None

    @staticmethod
    def _entry_page_subroute(path: str) -> tuple[int, str] | None:
        parts = path.strip("/").split("/")
        if len(parts) != 3 or parts[0] != "entry":
            return None
        if parts[2] not in {"action", "summarize", "export"}:
            return None
        try:
            return int(parts[1]), parts[2]
        except ValueError:
            return None

    def _send_json(
        self,
        payload: Any,
        status: HTTPStatus = HTTPStatus.OK,
    ) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _send_error(self, status: HTTPStatus, message: str) -> None:
        self._send_json({"error": message}, status)

    def _send_html(
        self, document: str, status: HTTPStatus = HTTPStatus.OK
    ) -> None:
        body = document.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: object) -> None:
        # Keep normal use quiet; server startup still reports the address.
        return


def make_server(
    settings: Settings,
    database: Database,
    *,
    host: str | None = None,
    port: int | None = None,
    service: HeraldService | None = None,
) -> HeraldServer:
    active_service = service or HeraldService(
        database,
        summarizer=LocalSummarizer(settings.ollama_url, settings.ollama_model),
        exporter=ObsidianExporter(settings.vault_path),
    )
    return HeraldServer(
        (host if host is not None else settings.host, port if port is not None else settings.port),
        database,
        settings,
        active_service,
    )


def serve(settings: Settings, database: Database) -> None:
    server = make_server(settings, database)
    address, port = server.server_address[:2]
    print(f"Herald is running at http://{address}:{port}")
    print("Press Ctrl+C to stop.")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping Herald.")
    finally:
        server.server_close()

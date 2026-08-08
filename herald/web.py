from __future__ import annotations

import json
import mimetypes
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from .config import Settings
from .obsidian import ObsidianExporter
from .papers import (
    PaperFetchError,
    PaperImportError,
    PaperNotFoundError,
    UnsafePaperUrlError,
)
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
            self._redirect_to_inbox()
            return
        entry_id = self._entry_id(parsed.path)
        if entry_id is not None:
            self._get_entry(entry_id)
            return
        self._send_error(HTTPStatus.NOT_FOUND, "Not found")

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        path = urlparse(self.path).path
        if path == "/api/sources":
            self._add_source()
            return
        if path == "/api/refresh":
            self._refresh()
            return
        if path == "/api/import/paper":
            self._import_paper()
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

    def _redirect_to_inbox(self) -> None:
        self.send_response(HTTPStatus.SEE_OTHER)
        self.send_header("Location", "/")
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

    def _import_paper(self) -> None:
        payload = self._read_json()
        if payload is None:
            return
        value = self._required_text(payload, "input")
        if not value:
            self._send_error(HTTPStatus.BAD_REQUEST, "input is required")
            return
        try:
            result = self.server.service.import_paper(value)
        except (ValueError, UnsafePaperUrlError) as error:
            self._send_error(HTTPStatus.BAD_REQUEST, str(error))
            return
        except PaperNotFoundError as error:
            self._send_error(HTTPStatus.NOT_FOUND, str(error))
            return
        except PaperFetchError as error:
            self._send_error(HTTPStatus.BAD_GATEWAY, str(error))
            return
        except PaperImportError as error:
            self._send_error(HTTPStatus.UNPROCESSABLE_ENTITY, str(error))
            return
        self._send_json(
            result.to_dict(),
            HTTPStatus.CREATED if result.created else HTTPStatus.OK,
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

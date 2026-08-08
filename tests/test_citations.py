from __future__ import annotations

import json
import tempfile
import threading
import unittest
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import unquote
from urllib.request import Request, urlopen

from herald.config import Settings
from herald.papers import PaperFetchError, PaperImporter
from herald.service import HeraldService
from herald.storage import Database
from herald.web import make_server


TARGET_ID = "2222222222222222222222222222222222222222"
GRANDCHILD_ID = "3333333333333333333333333333333333333333"


def paper_payload(
    doi: str,
    title: str,
    paper_id: str,
    *,
    references: list[dict[str, object]] | None = None,
) -> dict[str, object]:
    return {
        "paperId": paper_id,
        "externalIds": {"DOI": doi},
        "url": f"https://www.semanticscholar.org/paper/{paper_id}",
        "title": title,
        "abstract": f"Abstract for {title}.",
        "authors": [{"name": "Citation Author"}],
        "publicationDate": "2026-08-01",
        "fieldsOfStudy": ["Computer Science"],
        "references": references or [],
    }


CITING = paper_payload(
    "10.1000/citing",
    "The citing paper",
    "1111111111111111111111111111111111111111",
    references=[
        {
            "paperId": TARGET_ID.upper(),
            "externalIds": {"DOI": "10.1000/TARGET"},
            "url": f"https://www.semanticscholar.org/paper/{TARGET_ID}",
            "title": "The cited target",
        }
    ],
)
TARGET = paper_payload(
    "10.1000/target",
    "The cited target",
    TARGET_ID,
    references=[
        {
            "paperId": GRANDCHILD_ID,
            "externalIds": {"DOI": "10.1000/grandchild"},
            "url": f"https://www.semanticscholar.org/paper/{GRANDCHILD_ID}",
            "title": "A reference of the target",
        }
    ],
)


class RoutingFetcher:
    def __init__(self, *, fail_target: bool = False):
        self.fail_target = fail_target

    def __call__(self, url: str, headers: object, max_bytes: int, timeout: float) -> bytes:
        decoded = unquote(url).lower()
        if "10.1000/target" in decoded:
            if self.fail_target:
                raise PaperFetchError("metadata provider offline", transient=True)
            return json.dumps(TARGET).encode()
        if "10.1000/citing" in decoded:
            return json.dumps(CITING).encode()
        raise AssertionError(f"unexpected provider request: {url}")


class CitationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.root = Path(self.temporary_directory.name)
        self.database = Database(self.root / "herald.db")
        self.database.initialize()

    def importer(self, fetcher: RoutingFetcher) -> PaperImporter:
        return PaperImporter(
            self.database,
            fetcher=fetcher,
            sleep=lambda _: None,
            request_delay=0,
            retries=0,
        )

    def test_identifier_arrival_reconciles_one_directed_edge_idempotently(self) -> None:
        source_id = self.database.add_source("Papers", "https://example.org/feed", "Research")
        citing_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="citing",
            url="https://example.org/citing",
            title="Citing",
        )
        reference_id = self.database.upsert_paper_reference(
            citing_id,
            "doi:10.1000/target",
            external_scheme="DOI",
            external_id="10.1000/TARGET",
            cited_title="Target",
        )
        self.assertIsNone(self.database.get_paper_reference(reference_id)["cited_entry_id"])

        target_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="target",
            url="https://example.org/target",
            title="Target",
        )
        self.database.add_paper_identifier(target_id, "doi", "10.1000/target")

        reference = self.database.get_paper_reference(reference_id)
        self.assertEqual(reference["cited_entry_id"], target_id)
        self.assertEqual(self.database.list_paper_references(target_id), [])
        duplicate_id = self.database.upsert_paper_reference(
            citing_id,
            "doi:10.1000/target",
            external_scheme="doi",
            external_id="10.1000/target",
            cited_title="Target updated",
        )
        self.assertEqual(duplicate_id, reference_id)
        self.assertEqual(len(self.database.list_paper_references(citing_id)), 1)

    def test_add_imports_only_selected_target_as_unread_and_preserves_repeat_status(self) -> None:
        importer = self.importer(RoutingFetcher())
        citing = importer.import_paper("10.1000/citing")
        reference = citing.references[0]
        self.assertIsNone(reference["cited_entry_id"])
        self.assertEqual(len(self.database.list_entries(limit=None)), 1)

        service = HeraldService(self.database, paper_importer=importer)
        added = service.add_paper_reference(int(reference["id"]))
        target = added["import"]["entry"]

        self.assertTrue(added["import"]["created"])
        self.assertEqual(target["status"], "unread")
        self.assertIsNone(target["exported_path"])
        self.assertIsNone(self.database.get_obsidian_export(int(target["id"])))
        self.assertEqual(len(self.database.list_entries(limit=None)), 2)
        self.assertEqual(
            self.database.list_paper_references(int(target["id"]))[0]["external_id"],
            "10.1000/grandchild",
        )
        self.assertIsNone(
            self.database.list_paper_references(int(target["id"]))[0]["cited_entry_id"]
        )

        self.database.set_status(int(target["id"]), "discarded")
        repeated = service.add_paper_reference(int(reference["id"]))
        self.assertFalse(repeated["import"]["created"])
        self.assertEqual(repeated["import"]["entry"]["status"], "discarded")
        self.assertEqual(len(self.database.list_entries(limit=None)), 2)

    def test_provider_failure_leaves_reference_unresolved_and_database_unchanged(self) -> None:
        citing = self.importer(RoutingFetcher()).import_paper("10.1000/citing")
        reference_id = int(citing.references[0]["id"])
        service = HeraldService(
            self.database,
            paper_importer=self.importer(RoutingFetcher(fail_target=True)),
        )

        with self.assertRaises(PaperFetchError):
            service.add_paper_reference(reference_id)

        self.assertIsNone(self.database.get_paper_reference(reference_id)["cited_entry_id"])
        self.assertEqual(len(self.database.list_entries(limit=None)), 1)

    def test_reference_http_apis_list_and_add_the_selected_edge(self) -> None:
        importer = self.importer(RoutingFetcher())
        citing = importer.import_paper("10.1000/citing")
        reference_id = int(citing.references[0]["id"])
        settings = Settings(
            data_dir=self.root,
            database_path=self.database.path,
            vault_path=self.root / "vault",
            host="127.0.0.1",
            port=0,
            ollama_url="http://127.0.0.1:11434",
            ollama_model="demo",
        )
        service = HeraldService(
            self.database,
            paper_importer=importer,
            default_vault_path=settings.vault_path,
        )
        server = make_server(settings, self.database, port=0, service=service)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(lambda: thread.join(timeout=2))
        self.addCleanup(server.shutdown)
        base_url = f"http://127.0.0.1:{server.server_address[1]}"

        with urlopen(base_url + f"/api/entries/{citing.entry['id']}/references") as response:
            payload = json.loads(response.read())
        self.assertEqual(response.status, 200)
        self.assertEqual(payload["references"][0]["id"], reference_id)

        request = Request(
            base_url + f"/api/references/{reference_id}/add",
            data=b"{}",
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urlopen(request) as response:
            added = json.loads(response.read())
        self.assertEqual(response.status, 201)
        self.assertEqual(added["import"]["entry"]["status"], "unread")
        self.assertEqual(added["reference"]["cited_entry_id"], added["import"]["entry"]["id"])


if __name__ == "__main__":
    unittest.main()

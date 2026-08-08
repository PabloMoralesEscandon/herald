from __future__ import annotations

import argparse
import json
from typing import Sequence

from .config import Settings
from .demo import load_demo
from .service import HeraldService
from .storage import Database
from .summaries import LocalSummarizer


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="herald", description="Local-first research and news inbox"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("init", help="Create the local database and vault")
    subparsers.add_parser("demo", help="Load deterministic sample articles")
    subparsers.add_parser("list", help="Print current inbox entries")
    subparsers.add_parser("sources", help="Print configured RSS and Atom sources")
    subparsers.add_parser("seed", help="Add Herald's curated research sources")
    backfill = subparsers.add_parser(
        "backfill", help="Backfill identifiers, keywords, rankings, and kept notes"
    )
    backfill.add_argument(
        "--no-obsidian",
        action="store_true",
        help="Skip kept-note reconciliation for this run",
    )
    subparsers.add_parser("refresh", help="Fetch all enabled sources")
    summarize = subparsers.add_parser("summarize", help="Summarize one entry")
    summarize.add_argument("entry_id", type=int)
    export = subparsers.add_parser("export", help="Export one kept entry")
    export.add_argument("entry_id", type=int)
    subparsers.add_parser("export-kept", help="Export every kept entry")
    subparsers.add_parser("serve", help="Start the local web application")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    settings = Settings.from_env()
    settings.ensure_directories()
    database = Database(settings.database_path)
    database.initialize()
    service = HeraldService(
        database,
        summarizer=LocalSummarizer(settings.ollama_url, settings.ollama_model),
        default_vault_path=settings.vault_path,
        archive_root=settings.data_dir / "obsidian-archive",
    )

    if args.command == "init":
        created = service.seed_curated_sources()
        print(f"Herald initialized at {settings.data_dir} ({created} sources added)")
        return 0
    if args.command == "demo":
        created = load_demo(database)
        print(f"Loaded {created} demo entries")
        return 0
    if args.command == "list":
        print(json.dumps(database.list_entries(), indent=2))
        return 0
    if args.command == "sources":
        print(json.dumps(service.list_sources(), indent=2))
        return 0
    if args.command == "seed":
        created = service.seed_curated_sources()
        print(f"Added {created} curated sources")
        return 0
    if args.command == "backfill":
        print(
            json.dumps(
                service.backfill_existing(sync_obsidian=not args.no_obsidian),
                indent=2,
            )
        )
        return 0
    if args.command == "refresh":
        results = service.refresh_all()
        print(json.dumps([result.to_dict() for result in results], indent=2))
        return int(any(result.error for result in results))
    if args.command == "summarize":
        result = service.summarize_entry(args.entry_id)
        print(json.dumps({"summary": result.text, "provider": result.provider}))
        return 0
    if args.command == "export":
        print(service.export_entry(args.entry_id))
        return 0
    if args.command == "export-kept":
        paths = service.export_kept()
        print(json.dumps([str(path) for path in paths], indent=2))
        return 0
    if args.command == "serve":
        try:
            from .web import serve
        except ImportError:
            print("The web interface has not been installed yet.")
            return 2
        serve(settings, database)
        return 0
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

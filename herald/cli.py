from __future__ import annotations

import argparse
import json
from typing import Sequence

from .config import Settings
from .demo import load_demo
from .storage import Database


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="herald", description="Local-first research and news inbox"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("init", help="Create the local database and vault")
    subparsers.add_parser("demo", help="Load deterministic sample articles")
    subparsers.add_parser("list", help="Print current inbox entries")
    subparsers.add_parser("serve", help="Start the local web application")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    settings = Settings.from_env()
    settings.ensure_directories()
    database = Database(settings.database_path)
    database.initialize()

    if args.command == "init":
        print(f"Herald initialized at {settings.data_dir}")
        return 0
    if args.command == "demo":
        created = load_demo(database)
        print(f"Loaded {created} demo entries")
        return 0
    if args.command == "list":
        print(json.dumps(database.list_entries(), indent=2))
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


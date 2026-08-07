from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True, slots=True)
class Settings:
    data_dir: Path
    database_path: Path
    vault_path: Path
    host: str
    port: int
    ollama_url: str
    ollama_model: str

    @classmethod
    def from_env(cls) -> "Settings":
        data_dir = Path(os.environ.get("HERALD_DATA_DIR", ".herald")).expanduser()
        database_path = Path(
            os.environ.get("HERALD_DATABASE", str(data_dir / "herald.db"))
        ).expanduser()
        vault_path = Path(
            os.environ.get("HERALD_VAULT", str(data_dir / "vault"))
        ).expanduser()
        return cls(
            data_dir=data_dir,
            database_path=database_path,
            vault_path=vault_path,
            host=os.environ.get("HERALD_HOST", "127.0.0.1"),
            port=int(os.environ.get("HERALD_PORT", "8765")),
            ollama_url=os.environ.get("HERALD_OLLAMA_URL", "http://127.0.0.1:11434"),
            ollama_model=os.environ.get("HERALD_OLLAMA_MODEL", "qwen2.5:3b"),
        )

    def ensure_directories(self) -> None:
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.database_path.parent.mkdir(parents=True, exist_ok=True)
        self.vault_path.mkdir(parents=True, exist_ok=True)


from __future__ import annotations

from .storage import Database


DEMO_ENTRIES = (
    {
        "guid": "demo-chiplet-interconnect",
        "url": "https://example.org/research/chiplet-interconnect",
        "title": "A Low-Latency Interconnect for Modular Chiplets",
        "author": "Demo Research Group",
        "published_at": "2026-08-06T10:00:00+00:00",
        "content": (
            "The authors present a digital chiplet interconnect optimized for short "
            "reach links. The design trades a small amount of area for lower latency "
            "and evaluates the result across several packaging configurations."
        ),
        "summary": (
            "A proposed chiplet link reduces communication latency by trading a small "
            "amount of silicon area. The evaluation compares multiple package layouts."
        ),
    },
    {
        "guid": "demo-rl-scheduler",
        "url": "https://example.org/research/rl-scheduler",
        "title": "Reinforcement Learning for Operating-System Scheduling",
        "author": "Demo Systems Lab",
        "published_at": "2026-08-05T09:00:00+00:00",
        "content": (
            "This paper applies offline reinforcement learning to heterogeneous-core "
            "scheduling. It reports throughput and tail-latency results against Linux "
            "schedulers on reproducible workloads."
        ),
        "summary": (
            "Offline RL selects tasks for heterogeneous CPU cores. Reported results "
            "compare throughput and tail latency with standard Linux schedulers."
        ),
    },
)


def load_demo(database: Database) -> int:
    source_id = database.add_source(
        "Herald Demo Research", "https://example.org/herald-demo.xml", "Demo"
    )
    created = 0
    for entry in DEMO_ENTRIES:
        entry_id, was_created = database.upsert_entry(
            source_id=source_id, summary_provider="demo", **entry
        )
        # Demo data is complete and must remain fully offline when triaged.
        database.set_enrichment_state(entry_id, "enriched", provider="demo")
        created += int(was_created)
    return created

package main

import (
	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// demoEntries are deterministic sample articles for an offline first look.
// They never contact a feed and are complete enough to triage and export.
var demoEntries = []store.UpsertEntryInput{
	{
		GUID:   "demo-chiplet-interconnect",
		URL:    "https://example.org/research/chiplet-interconnect",
		Title:  "A Low-Latency Interconnect for Modular Chiplets",
		Author: "Demo Research Group",
		Content: "The authors present a digital chiplet interconnect optimized for short " +
			"reach links. The design trades a small amount of area for lower latency " +
			"and evaluates the result across several packaging configurations.",
		Summary: "A proposed chiplet link reduces communication latency by trading a small " +
			"amount of silicon area. The evaluation compares multiple package layouts.",
		SummaryProvider: "demo",
	},
	{
		GUID:   "demo-rl-scheduler",
		URL:    "https://example.org/research/rl-scheduler",
		Title:  "Reinforcement Learning for Operating-System Scheduling",
		Author: "Demo Systems Lab",
		Content: "This paper applies offline reinforcement learning to heterogeneous-core " +
			"scheduling. It reports throughput and tail-latency results against Linux " +
			"schedulers on reproducible workloads.",
		Summary: "Offline RL selects tasks for heterogeneous CPU cores. Reported results " +
			"compare throughput and tail latency with standard Linux schedulers.",
		SummaryProvider: "demo",
	},
}

var demoPublished = []string{
	"2026-08-06T10:00:00+00:00",
	"2026-08-05T09:00:00+00:00",
}

// loadDemo adds the sample entries. Running it twice adds nothing new.
func loadDemo(db *store.DB) (int, error) {
	sourceID, err := db.AddSource(store.AddSourceInput{
		Title: "Herald Demo Research", URL: "https://example.org/herald-demo.xml",
		Category: "Demo", ContentKind: "paper",
	})
	if err != nil {
		return 0, err
	}
	created := 0
	for index, entry := range demoEntries {
		entry.SourceID = sourceID
		published := demoPublished[index]
		entry.PublishedAt = &published
		entryID, wasCreated, err := db.UpsertEntry(entry)
		if err != nil {
			return created, err
		}
		// Demo data is complete, so it must stay fully offline when triaged.
		if _, err := db.SetEnrichmentState(entryID, "enriched",
			store.EnrichmentState{Provider: "demo"}); err != nil {
			return created, err
		}
		if wasCreated {
			created++
		}
	}
	return created, nil
}

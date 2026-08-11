package relevance

import (
	"fmt"
	"sync"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// Job is the observable state of one background rescore.
type Job struct {
	ContentKind    string         `json:"content_kind"`
	State          string         `json:"state"`
	StartedAt      string         `json:"started_at"`
	FinishedAt     *string        `json:"finished_at"`
	Result         *RescoreResult `json:"result"`
	Error          *string        `json:"error"`
	RerunRequested bool           `json:"rerun_requested"`
}

// Coordinator runs at most one rescore per content kind and keeps the outcome
// observable through /api/relevance/health.
type Coordinator struct {
	Engine *Engine

	mutex sync.Mutex
	jobs  map[string]*Job
	// waiters lets tests and shutdown wait for in-flight work.
	running sync.WaitGroup
}

// NewCoordinator wires a coordinator to an engine.
func NewCoordinator(engine *Engine) *Coordinator {
	return &Coordinator{Engine: engine, jobs: map[string]*Job{}}
}

// Start schedules a rescore and returns a snapshot of the job.
//
// A request arriving while one is already running does not queue a second pass;
// it marks a rerun so the in-flight scoring is repeated once with the newer
// profile, which keeps rapid profile edits from piling up work.
func (c *Coordinator) Start(contentKind string) (*Job, error) {
	if contentKind != "paper" && contentKind != "news" {
		return nil, fmt.Errorf("Unknown content kind: %s", contentKind)
	}
	c.mutex.Lock()
	if current, ok := c.jobs[contentKind]; ok && current.State == "running" {
		current.RerunRequested = true
		snapshot := *current
		c.mutex.Unlock()
		return &snapshot, nil
	}
	job := &Job{
		ContentKind: contentKind,
		State:       "running",
		StartedAt:   store.UTCNow(),
	}
	c.jobs[contentKind] = job
	snapshot := *job
	c.running.Add(1)
	c.mutex.Unlock()

	go c.run(contentKind)
	return &snapshot, nil
}

func (c *Coordinator) run(contentKind string) {
	defer c.running.Done()
	result, err := c.Engine.Rescore(contentKind)

	c.mutex.Lock()
	job := c.jobs[contentKind]
	rerun := job.RerunRequested
	finishedAt := store.UTCNow()
	job.FinishedAt = &finishedAt
	job.RerunRequested = false
	if err != nil {
		// A background failure must stay visible rather than disappearing.
		message := err.Error()
		job.State = "failed"
		job.Error = &message
	} else {
		job.State = "complete"
		job.Result = result
	}
	c.mutex.Unlock()

	if rerun {
		c.Start(contentKind)
	}
}

// Wait blocks until no rescore is in flight. It exists for tests and for an
// orderly shutdown.
func (c *Coordinator) Wait() { c.running.Wait() }

// Health reports the providers, jobs, and current profiles.
type Health struct {
	PreferredProvider string           `json:"preferred_provider"`
	FallbackProvider  string           `json:"fallback_provider"`
	Jobs              map[string]*Job  `json:"jobs"`
	Profiles          []*store.Profile `json:"profiles"`
}

// Health returns the current provider and job state.
func (c *Coordinator) Health() (*Health, error) {
	c.mutex.Lock()
	jobs := make(map[string]*Job, len(c.jobs))
	for kind, job := range c.jobs {
		snapshot := *job
		jobs[kind] = &snapshot
	}
	c.mutex.Unlock()

	preferred := "tfidf-v1"
	if c.Engine.Embedder != nil {
		preferred = c.Engine.Embedder.Model()
	}
	profiles, err := c.Engine.DB.ListRelevanceProfiles()
	if err != nil {
		return nil, err
	}
	return &Health{
		PreferredProvider: preferred,
		FallbackProvider:  "tfidf-v1",
		Jobs:              jobs,
		Profiles:          profiles,
	}, nil
}

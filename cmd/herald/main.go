// Command herald is a local-first inbox for technical news and academic
// research.
//
// It ships as a single binary with the dashboard and source catalog embedded,
// so there is nothing to install alongside it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/api"
	"github.com/PabloMoralesEscandon/herald/internal/config"
	"github.com/PabloMoralesEscandon/herald/internal/relevance"
	"github.com/PabloMoralesEscandon/herald/internal/service"
	"github.com/PabloMoralesEscandon/herald/internal/sources"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
)

const usage = `herald - local-first research and news inbox

Usage: herald <command> [options]

Commands:
  init                    Create the local database and vault
  demo                    Load deterministic sample articles
  list                    Print current inbox entries
  sources [export|import] List, export, or import source configuration
  seed                    Add Herald's packaged source catalog
  backfill [--no-obsidian]
                          Backfill identifiers, keywords, rankings, and notes
  refresh                 Fetch all enabled sources
  summarize <entry-id>    Summarize one entry
  export <entry-id>       Export one kept entry
  export-kept             Export every kept entry
  serve                   Start the local web application
`

func main() {
	if code := run(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	command := args[0]
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Print(usage)
		return 0
	}

	settings := config.FromEnv()
	if err := settings.EnsureDirectories(); err != nil {
		return fail("could not create local directories: %v", err)
	}
	db, err := store.Open(settings.DatabasePath)
	if err != nil {
		return fail("could not open the database: %v", err)
	}
	defer db.Close()
	if err := db.Initialize(); err != nil {
		return fail("could not initialize the database: %v", err)
	}

	// The CLI runs offline by default so scripted use is deterministic and
	// never blocks on a local model; `serve` opts into the model and
	// background enrichment.
	engine, err := relevance.NewEngine(db, nil)
	if err != nil {
		return fail("could not prepare relevance profiles: %v", err)
	}
	svc := service.New(db, service.Options{
		Summarizer:   summary.NewLocalSummarizer(settings.OllamaURL, settings.OllamaModel),
		Relevance:    relevance.NewCoordinator(engine),
		DefaultVault: settings.VaultPath,
		ArchiveRoot:  settings.ArchiveRoot(),
	})

	switch command {
	case "init":
		created, err := svc.SeedCuratedSources()
		if err != nil {
			return fail("could not seed sources: %v", err)
		}
		fmt.Printf("Herald initialized at %s (%d sources added)\n", settings.DataDir, created)
		return 0

	case "demo":
		created, err := loadDemo(db)
		if err != nil {
			return fail("could not load the demo: %v", err)
		}
		fmt.Printf("Loaded %d demo entries\n", created)
		return 0

	case "list":
		entries, err := db.ListEntries(store.EntryFilter{Limit: 100})
		if err != nil {
			return fail("could not list entries: %v", err)
		}
		return printJSON(entries)

	case "sources":
		return runSources(svc, db, args[1:])

	case "seed":
		created, err := svc.SeedCuratedSources()
		if err != nil {
			return fail("could not seed sources: %v", err)
		}
		fmt.Printf("Added %d curated sources\n", created)
		return 0

	case "backfill":
		flags := flag.NewFlagSet("backfill", flag.ContinueOnError)
		noObsidian := flags.Bool("no-obsidian", false,
			"skip kept-note reconciliation for this run")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		result, err := svc.Backfill(!*noObsidian)
		if err != nil {
			return fail("backfill failed: %v", err)
		}
		return printJSON(result)

	case "refresh":
		results, err := svc.RefreshAll()
		if err != nil {
			return fail("refresh failed: %v", err)
		}
		if code := printJSON(results); code != 0 {
			return code
		}
		// A non-zero exit signals that at least one feed failed, so scripts
		// and cron jobs can notice.
		for _, result := range results {
			if result.Error != nil {
				return 1
			}
		}
		return 0

	case "summarize":
		entryID, err := requireID(args[1:], "summarize")
		if err != nil {
			return fail("%v", err)
		}
		result, err := svc.SummarizeEntry(entryID)
		if err != nil {
			return fail("could not summarize: %v", err)
		}
		return printJSON(map[string]any{
			"summary": result.Text, "provider": result.Provider,
		})

	case "export":
		entryID, err := requireID(args[1:], "export")
		if err != nil {
			return fail("%v", err)
		}
		path, err := svc.ExportEntry(entryID)
		if err != nil {
			return fail("could not export: %v", err)
		}
		fmt.Println(path)
		return 0

	case "export-kept":
		paths, err := svc.ExportKept()
		if err != nil {
			return fail("could not export kept entries: %v", err)
		}
		if paths == nil {
			paths = []string{}
		}
		return printJSON(paths)

	case "serve":
		return serve(settings, db)
	}

	fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", command, usage)
	return 2
}

// serve starts the dashboard with the model-backed collaborators enabled.
func serve(settings config.Settings, db *store.DB) int {
	engine, err := relevance.NewEngine(db,
		relevance.NewOllamaEmbedder(db, settings.OllamaURL, settings.OllamaEmbeddingModel))
	if err != nil {
		return fail("could not prepare relevance profiles: %v", err)
	}
	svc := service.New(db, service.Options{
		Summarizer:   summary.NewLocalSummarizer(settings.OllamaURL, settings.OllamaModel),
		Relevance:    relevance.NewCoordinator(engine),
		DefaultVault: settings.VaultPath,
		ArchiveRoot:  settings.ArchiveRoot(),
		// Keeping a paper enriches it in the background while the dashboard
		// stays responsive.
		AutoEnrichKept: true,
	})
	if err := svc.ReconcileObsidian(); err != nil {
		return fail("could not reconcile the vault: %v", err)
	}

	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	server := &http.Server{
		Addr:              address,
		Handler:           api.New(db, svc),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fail("could not bind %s: %v", address, err)
	}

	fmt.Printf("Herald is running at http://%s\n", listener.Addr())
	fmt.Println("Press Ctrl+C to stop.")

	// Ctrl+C drains in-flight requests, then waits for background scoring and
	// enrichment so no half-written note is left behind.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return fail("server error: %v", err)
	case <-stop:
		fmt.Println("\nStopping Herald.")
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown: %v\n", err)
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
	return 0
}

// runSources implements the list/export/import subcommands.
func runSources(svc *service.Service, db *store.DB, args []string) int {
	if len(args) == 0 {
		sourceRows, err := db.ListSources(false)
		if err != nil {
			return fail("could not list sources: %v", err)
		}
		return printJSON(sourceRows)
	}
	action := args[0]
	path := ""
	if len(args) > 1 {
		path = args[1]
	}

	switch action {
	case "export":
		manifest, err := svc.ExportSources()
		if err != nil {
			return fail("could not export sources: %v", err)
		}
		document, err := encodeJSON(manifest)
		if err != nil {
			return fail("could not encode sources: %v", err)
		}
		if path == "" || path == "-" {
			os.Stdout.Write(document)
			return 0
		}
		destination := expandHome(path)
		if err := os.WriteFile(destination, document, 0o644); err != nil {
			return fail("could not write %s: %v", destination, err)
		}
		fmt.Printf("Exported %d sources to %s\n", len(manifest.Sources), destination)
		return 0

	case "import":
		if path == "" {
			fmt.Fprintln(os.Stderr,
				"herald sources import requires a JSON path (or '-' for stdin)")
			return 2
		}
		document, err := readManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not import sources: %v\n", err)
			return 2
		}
		result, err := svc.ImportSources(document)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not import sources: %v\n", err)
			return 2
		}
		return printJSON(result)
	}
	fmt.Fprintf(os.Stderr, "unknown sources action: %s\n", action)
	return 2
}

// readManifest reads a manifest from a file or stdin, enforcing the size cap
// before anything is parsed.
func readManifest(path string) ([]byte, error) {
	if path == "-" {
		document, err := io.ReadAll(io.LimitReader(os.Stdin, sources.MaxManifestBytes+1))
		if err != nil {
			return nil, err
		}
		if len(document) > sources.MaxManifestBytes {
			return nil, fmt.Errorf("Source manifest exceeds the 1 MB limit")
		}
		return document, nil
	}
	source := expandHome(path)
	info, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if info.Size() > sources.MaxManifestBytes {
		return nil, fmt.Errorf("Source manifest exceeds the 1 MB limit")
	}
	return os.ReadFile(source)
}

func requireID(args []string, command string) (int64, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("%s requires an entry id", command)
	}
	entryID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid entry id", args[0])
	}
	return entryID, nil
}

// encodeJSON renders indented JSON without HTML escaping.
//
// Go escapes &, <, and > by default, which would turn category names such as
// "Chip Design & Electronics" into "Chip Design \u0026 Electronics" in an
// exported manifest.
func encodeJSON(payload any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func printJSON(payload any) int {
	document, err := encodeJSON(payload)
	if err != nil {
		return fail("could not encode output: %v", err)
	}
	os.Stdout.Write(document)
	return 0
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	return 1
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

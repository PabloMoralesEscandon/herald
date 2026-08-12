// Package config resolves Herald's local runtime settings from the environment.
//
// Herald deliberately does not read .env files itself; copy .env.example and
// load it through your shell or process manager.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Settings holds every path and endpoint Herald needs to run locally.
type Settings struct {
	DataDir              string
	DatabasePath         string
	VaultPath            string
	Host                 string
	Port                 int
	OllamaURL            string
	OllamaModel          string
	OllamaEmbeddingModel string
}

// Defaults mirrors the documented environment variable defaults.
const (
	DefaultDataDir              = ".herald"
	DefaultHost                 = "127.0.0.1"
	DefaultPort                 = 8765
	DefaultOllamaURL            = "http://127.0.0.1:11434"
	DefaultOllamaModel          = "qwen2.5:3b"
	DefaultOllamaEmbeddingModel = "embeddinggemma"
)

// FromEnv builds settings from HERALD_* variables, falling back to the local
// defaults documented in README.md.
func FromEnv() Settings {
	dataDir := expand(envOr("HERALD_DATA_DIR", DefaultDataDir))
	port, err := strconv.Atoi(envOr("HERALD_PORT", strconv.Itoa(DefaultPort)))
	if err != nil {
		port = DefaultPort
	}
	return Settings{
		DataDir:              dataDir,
		DatabasePath:         expand(envOr("HERALD_DATABASE", filepath.Join(dataDir, "herald.db"))),
		VaultPath:            expand(envOr("HERALD_VAULT", filepath.Join(dataDir, "vault"))),
		Host:                 envOr("HERALD_HOST", DefaultHost),
		Port:                 port,
		OllamaURL:            envOr("HERALD_OLLAMA_URL", DefaultOllamaURL),
		OllamaModel:          envOr("HERALD_OLLAMA_MODEL", DefaultOllamaModel),
		OllamaEmbeddingModel: envOr("HERALD_OLLAMA_EMBEDDING_MODEL", DefaultOllamaEmbeddingModel),
	}
}

// ArchiveRoot is where notes removed from the vault remain recoverable.
func (s Settings) ArchiveRoot() string {
	return filepath.Join(s.DataDir, "obsidian-archive")
}

// PDFRoot is where the PDFs an extraction read, and the ones a user uploaded,
// are kept. They live outside the vault: they are Herald's working files, not
// notes, and an Obsidian vault full of binaries is nobody's idea of a vault.
func (s Settings) PDFRoot() string {
	return filepath.Join(s.DataDir, "pdfs")
}

// EnsureDirectories creates the data, database, and vault directories.
func (s Settings) EnsureDirectories() error {
	for _, dir := range []string{s.DataDir, filepath.Dir(s.DatabasePath), s.VaultPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}

// expand resolves a leading ~ the way Python's Path.expanduser does.
func expand(path string) string {
	if path == "~" || len(path) > 1 && path[0] == '~' && os.IsPathSeparator(path[1]) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

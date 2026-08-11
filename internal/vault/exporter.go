package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ExportResult describes a written note.
type ExportResult struct {
	Path           string
	ContentHash    string
	MigratedLegacy bool
}

// ArchiveResult describes a note moved out of the vault.
type ArchiveResult struct {
	OriginalPath string
	ArchivePath  string
}

// Exporter writes notes into a vault and archives them out of it.
type Exporter struct {
	VaultPath   string
	ArchiveRoot string
}

// NewExporter validates that the archive cannot live inside the vault.
//
// Keeping them separate matters: an archive nested in the vault would reappear
// in Obsidian as duplicate notes of everything the user has un-kept.
func NewExporter(vaultPath, archiveRoot string) (*Exporter, error) {
	vault, err := resolvePath(vaultPath)
	if err != nil {
		return nil, err
	}
	if archiveRoot == "" {
		archiveRoot = filepath.Join(filepath.Dir(vault), "obsidian-archive")
	}
	archive, err := resolvePath(archiveRoot)
	if err != nil {
		return nil, err
	}
	if vault == archive {
		return nil, fmt.Errorf("The vault and archive paths must be different")
	}
	if isAncestor(vault, archive) || isAncestor(archive, vault) {
		return nil, fmt.Errorf("The archive must be outside the Obsidian vault")
	}
	return &Exporter{VaultPath: vault, ArchiveRoot: archive}, nil
}

func resolvePath(path string) (string, error) {
	expanded, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	// Resolve symlinks in the part of the path that exists, so containment
	// checks compare real locations.
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		return resolved, nil
	}
	return filepath.Clean(expanded), nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

func isAncestor(ancestor, descendant string) bool {
	relative, err := filepath.Rel(ancestor, descendant)
	if err != nil {
		return false
	}
	return relative != "." && !strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative)
}

// assertSafe rejects any path that escapes its managed root or traverses a
// symbolic link.
//
// Both checks are required. Containment alone would still allow a symlink
// inside the vault to redirect a write to an arbitrary location on disk, and
// the recorded note path is data that may have been edited by hand.
func (e *Exporter) assertSafe(path, root string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	cleaned := filepath.Clean(absolute)
	relative, err := filepath.Rel(root, cleaned)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("Path escapes managed root: %s", path)
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Symbolic links are not allowed in managed paths: %s", path)
		}
	}
	return nil
}

// Destination returns where a note belongs.
//
// A note that already lives at a valid stable path stays there, so a note is
// never silently relocated once the user has it open in Obsidian.
func (e *Exporter) Destination(note *Note) (string, error) {
	if existing := strings.TrimSpace(note.ObsidianRelativePath); existing != "" {
		if candidate, ok := e.stablePath(note, existing); ok {
			return candidate, nil
		}
	}
	key := StableKey(note)
	var relative string
	if note.ContentKind == "news" {
		publisher := safeComponent(note.SourceTitle, "Unknown Publisher", 100)
		relative = filepath.Join("Herald", "News", publisher, key+".md")
	} else {
		relative = filepath.Join("Herald", "Papers", key+".md")
	}
	destination := filepath.Join(e.VaultPath, relative)
	return destination, e.assertSafe(destination, e.VaultPath)
}

// stablePath reports whether an existing recorded path is one Herald would
// itself have chosen, and is therefore safe to keep using.
func (e *Exporter) stablePath(note *Note, existing string) (string, bool) {
	if filepath.IsAbs(existing) || filepath.Ext(existing) != ".md" {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(existing)), "/")
	stablePaper := note.ContentKind != "news" && len(parts) == 3 &&
		parts[0] == "Herald" && parts[1] == "Papers"
	stableNews := note.ContentKind == "news" && len(parts) == 4 &&
		parts[0] == "Herald" && parts[1] == "News"
	if !stablePaper && !stableNews {
		return "", false
	}
	candidate := filepath.Join(e.VaultPath, filepath.FromSlash(existing))
	if err := e.assertSafe(candidate, e.VaultPath); err != nil {
		return "", false
	}
	return candidate, true
}

// Export writes or updates a note.
func (e *Exporter) Export(note *Note) (*ExportResult, error) {
	if note.Status != "kept" {
		return nil, fmt.Errorf("Only kept entries can be exported")
	}
	destination, err := e.Destination(note)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return nil, err
	}
	if err := e.assertSafe(destination, e.VaultPath); err != nil {
		return nil, err
	}

	migrated := false
	var document string
	info, statErr := os.Lstat(destination)
	switch {
	case statErr == nil && !info.Mode().IsRegular():
		return nil, &ConflictError{Reason: "The Obsidian destination is not a file"}
	case statErr == nil:
		existing, readErr := os.ReadFile(destination)
		if readErr != nil {
			return nil, readErr
		}
		text := string(existing)
		if strings.Contains(text, FrontmatterStart) || strings.Contains(text, BodyStart) {
			document, err = mergeManaged(text, note)
		} else {
			document, err = migrateLegacy(text, note)
			migrated = true
		}
		if err != nil {
			return nil, err
		}
	case os.IsNotExist(statErr):
		document = Render(note)
	default:
		return nil, statErr
	}

	if err := atomicWrite(destination, document); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(document))
	return &ExportResult{
		Path:           destination,
		ContentHash:    hex.EncodeToString(digest[:]),
		MigratedLegacy: migrated,
	}, nil
}

// Archive moves a note out of the vault into the recoverable archive.
func (e *Exporter) Archive(note *Note, relativePath string) (*ArchiveResult, error) {
	if relativePath == "" {
		return nil, nil
	}
	source := filepath.Join(e.VaultPath, filepath.FromSlash(relativePath))
	if err := e.assertSafe(source, e.VaultPath); err != nil {
		return nil, err
	}
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &ConflictError{Reason: "The Obsidian note is not a regular file"}
	}

	// The timestamp keeps repeated archive cycles of one note distinct.
	stamp := time.Now().UTC().Format("20060102T150405.000000") + "Z"
	destination := filepath.Join(e.ArchiveRoot, stamp, filepath.FromSlash(relativePath))
	if err := e.assertSafe(destination, e.ArchiveRoot); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(source, destination); err != nil {
		return nil, err
	}
	if err := os.Remove(source); err != nil {
		return nil, err
	}
	return &ArchiveResult{OriginalPath: source, ArchivePath: destination}, nil
}

// Restore brings an archived note, with its annotations, back into the vault.
func (e *Exporter) Restore(note *Note, archivePath string) (string, error) {
	source := filepath.Join(e.ArchiveRoot, filepath.FromSlash(archivePath))
	if err := e.assertSafe(source, e.ArchiveRoot); err != nil {
		return "", err
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	destination, err := e.Destination(note)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := e.assertSafe(destination, e.VaultPath); err != nil {
		return "", err
	}
	// Never clobber a file already at the destination; that would discard
	// whatever the user has there.
	if _, err := os.Lstat(destination); err == nil {
		return "", &ConflictError{
			Reason: "Cannot restore annotations because the destination already exists",
		}
	}
	if err := copyFile(source, destination); err != nil {
		return "", err
	}
	return destination, nil
}

// RelativePath expresses a vault path in the form stored in the database.
func (e *Exporter) RelativePath(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(e.VaultPath, filepath.Clean(resolved))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

// ArchiveRelativePath expresses an archive path in stored form.
func (e *Exporter) ArchiveRelativePath(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(e.ArchiveRoot, filepath.Clean(resolved))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

// atomicWrite replaces a note in one step.
//
// Writing through a temporary file and renaming means a crash or a full disk
// leaves the previous note intact rather than truncated.
func atomicWrite(destination, document string) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination),
		"."+filepath.Base(destination)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)

	if _, err := temporary.WriteString(document); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, destination)
}

func copyFile(source, destination string) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination),
		"."+filepath.Base(destination)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)

	input, err := os.Open(source)
	if err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		input.Close()
		temporary.Close()
		return err
	}
	input.Close()
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if info, err := os.Stat(source); err == nil {
		_ = os.Chmod(name, info.Mode())
	}
	return os.Rename(name, destination)
}

// replaceManagedBlock swaps one marked region, refusing anything ambiguous.
func replaceManagedBlock(document, start, end, replacement string) (string, error) {
	if strings.Count(document, start) != 1 || strings.Count(document, end) != 1 {
		return "", &ConflictError{
			Reason: "The note's Herald management markers are missing or ambiguous",
		}
	}
	startAt := strings.Index(document, start)
	endAt := strings.Index(document[startAt:], end)
	if endAt < 0 {
		return "", &ConflictError{Reason: "The note's Herald management markers are invalid"}
	}
	endAt += startAt + len(end)
	return document[:startAt] + replacement + document[endAt:], nil
}

// mergeManaged rewrites only Herald's regions, preserving everything else.
func mergeManaged(document string, note *Note) (string, error) {
	merged, err := replaceManagedBlock(document, FrontmatterStart, FrontmatterEnd,
		managedFrontmatter(note))
	if err != nil {
		return "", err
	}
	return replaceManagedBlock(merged, BodyStart, BodyEnd, managedBody(note))
}

// legacyKeys are the frontmatter properties Herald itself wrote in the original
// note format. Anything else in a legacy note is the user's and is preserved.
var legacyKeys = map[string]bool{
	"herald_id": true, "title": true, "source": true, "source_title": true,
	"category": true, "author": true, "published_at": true, "discovered_at": true,
	"status": true, "summary_provider": true, "summary_model": true,
	"summary_generated_at": true, "tags": true, "type": true, "publisher": true,
	"canonical_source": true, "updated_at": true, "keywords": true, "topics": true,
	"relevance_score": true, "relevance_bucket": true, "relevance_model": true,
	"relevance_scored_at": true, "relevance_reasons": true,
}

var (
	heraldIDPattern   = regexp.MustCompile(`(?m)^herald_id:\s*(\d+)\s*$`)
	frontmatterKey    = regexp.MustCompile(`^([A-Za-z0-9_-]+):`)
	listItemPattern   = regexp.MustCompile(`^\s+-\s+`)
	notesHeadingRegex = regexp.MustCompile(`(?m)^## (?:My )?Notes\s*$`)
)

// migrateLegacy upgrades a note written before the managed markers existed.
//
// It refuses unless it can prove the file is Herald's own note for this entry
// and can locate the user's Notes section; an ambiguous file becomes a conflict
// rather than being overwritten.
func migrateLegacy(document string, note *Note) (string, error) {
	if !strings.HasPrefix(document, "---\n") {
		return "", &ConflictError{Reason: "Existing file is not a Herald-managed note"}
	}
	frontmatterEnd := strings.Index(document[4:], "\n---\n")
	if frontmatterEnd < 0 {
		return "", &ConflictError{Reason: "Legacy note has malformed frontmatter"}
	}
	frontmatterEnd += 4
	oldFrontmatter := document[4:frontmatterEnd]

	match := heraldIDPattern.FindStringSubmatch(oldFrontmatter)
	if match == nil {
		return "", &ConflictError{Reason: "Existing file belongs to a different entry"}
	}
	if identifier, err := strconv.ParseInt(match[1], 10, 64); err != nil || identifier != note.ID {
		return "", &ConflictError{Reason: "Existing file belongs to a different entry"}
	}

	var preserved []string
	skippingTags := false
	for _, line := range strings.Split(oldFrontmatter, "\n") {
		if key := frontmatterKey.FindStringSubmatch(line); key != nil {
			skippingTags = key[1] == "tags"
			if legacyKeys[key[1]] {
				continue
			}
		} else if skippingTags && listItemPattern.MatchString(line) {
			continue
		} else {
			skippingTags = false
		}
		preserved = append(preserved, line)
	}

	body := document[frontmatterEnd+len("\n---\n"):]
	notesMatch := notesHeadingRegex.FindStringIndex(body)
	if notesMatch == nil {
		return "", &ConflictError{
			Reason: "Legacy Herald note has no Notes section; refusing to overwrite it",
		}
	}
	notes := body[notesMatch[1]:]

	var custom []string
	for _, line := range preserved {
		if strings.TrimSpace(line) != "" {
			custom = append(custom, line)
		}
	}
	frontmatter := managedFrontmatter(note)
	if len(custom) > 0 {
		frontmatter += "\n" + strings.Join(custom, "\n")
	}
	return "---\n" + frontmatter + "\n---\n\n" + managedBody(note) +
		"\n\n## My Notes" + notes, nil
}

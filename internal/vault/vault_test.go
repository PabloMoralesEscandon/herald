package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newExporter(t *testing.T) *Exporter {
	t.Helper()
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultPath, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	exporter, err := NewExporter(vaultPath, filepath.Join(root, "archive"))
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}
	return exporter
}

func keptNote() *Note {
	return &Note{
		ID: 7, Title: "A Low-Latency Chiplet Interconnect",
		URL: "https://example.org/a", CanonicalURL: "https://example.org/a",
		CanonicalKey: "doi:10.1145/example", Author: "Ada Researcher",
		PublishedAt: "2026-08-06T10:00:00+00:00", DiscoveredAt: "2026-08-06T11:00:00+00:00",
		Status: "kept", Content: "Body text.", Summary: "A summary.",
		SummaryProvider: "extractive", ContentKind: "paper",
		EnrichmentStatus: "enriched", SourceTitle: "Systems Lab",
		SourceCategory: "Chip Design",
	}
}

// TestArchiveMustLiveOutsideTheVault covers the constructor guarantee. An
// archive inside the vault would resurface every un-kept note in Obsidian.
func TestArchiveMustLiveOutsideTheVault(t *testing.T) {
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault")
	os.MkdirAll(vaultPath, 0o755)
	for _, archive := range []string{
		vaultPath,
		filepath.Join(vaultPath, "archive"),
		filepath.Join(vaultPath, "nested", "deeper", "archive"),
	} {
		if _, err := NewExporter(vaultPath, archive); err == nil {
			t.Errorf("NewExporter accepted archive %q inside the vault", archive)
		}
	}
}

// TestArchiveContainmentSurvivesSymlinkedPaths pins the fix for a real defect.
//
// Containment was checked by resolving symlinks only for paths that already
// existed. An existing vault was therefore compared in resolved form against an
// archive that did not exist yet in unresolved form, so an archive nested
// inside the vault was accepted. This reproduces the condition directly rather
// than relying on a platform whose temp directory happens to be symlinked —
// macOS (/var to /private/var) and Windows (8.3 short names) hit it, plain
// Linux temp directories do not.
func TestArchiveContainmentSurvivesSymlinkedPaths(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "vault"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The vault is reached through the symlink; the archive does not exist yet.
	vaultPath := filepath.Join(link, "vault")
	inside := filepath.Join(vaultPath, "archive")
	if _, err := NewExporter(vaultPath, inside); err == nil {
		t.Errorf("NewExporter accepted archive %q inside symlinked vault %q", inside, vaultPath)
	}

	// The mirror case: the vault named through the symlink and the archive
	// named through the real path must still be recognized as the same tree.
	realInside := filepath.Join(real, "vault", "archive")
	if _, err := NewExporter(vaultPath, realInside); err == nil {
		t.Errorf("NewExporter accepted archive %q inside the same vault named differently", realInside)
	}

	// A genuinely separate archive is still accepted, whichever spelling is
	// used, and both spellings must canonicalize to the same exporter.
	outside := filepath.Join(link, "archive")
	exporter, err := NewExporter(vaultPath, outside)
	if err != nil {
		t.Fatalf("NewExporter rejected a valid sibling archive: %v", err)
	}
	viaReal, err := NewExporter(filepath.Join(real, "vault"), filepath.Join(real, "archive"))
	if err != nil {
		t.Fatalf("NewExporter rejected the same layout by real path: %v", err)
	}
	if exporter.VaultPath != viaReal.VaultPath || exporter.ArchiveRoot != viaReal.ArchiveRoot {
		t.Errorf("the same layout canonicalized differently:\n  via link: %s | %s\n  via real: %s | %s",
			exporter.VaultPath, exporter.ArchiveRoot, viaReal.VaultPath, viaReal.ArchiveRoot)
	}
}

// TestExportIsIdempotentAndStablyNamed covers the promise that note names do
// not change with article titles and that re-export rewrites the same file.
func TestExportIsIdempotentAndStablyNamed(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	first, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	relative, err := exporter.RelativePath(first.Path)
	if err != nil {
		t.Fatalf("RelativePath: %v", err)
	}
	// Unsafe characters in the identity key become spaces, not hyphens.
	if relative != "Herald/Papers/doi 10.1145 example.md" {
		t.Errorf("note path is %q", relative)
	}

	// A changed title must not move the note.
	note.Title = "A Completely Different Title"
	note.ObsidianRelativePath = relative
	second, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("note moved from %q to %q after a title change", first.Path, second.Path)
	}
	if second.ContentHash == first.ContentHash {
		t.Error("the new title was not written into the note")
	}
}

// TestOnlyKeptEntriesExport covers the documented precondition.
func TestOnlyKeptEntriesExport(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	note.Status = "unread"
	if _, err := exporter.Export(note); err == nil {
		t.Error("Export accepted an entry that is not kept")
	}
}

// TestReexportPreservesCustomFrontmatterAndNotes is the core promise of the
// managed-block design: everything outside Herald's markers is the user's.
func TestReexportPreservesCustomFrontmatterAndNotes(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	result, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	original, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	edited := strings.Replace(string(original), "---\n"+FrontmatterStart,
		"---\nmy_rating: 5\nmy_project: \"thesis\"\n"+FrontmatterStart, 1)
	edited = strings.TrimRight(edited, "\n") + "\nMy own thoughts about this paper.\n"
	if err := os.WriteFile(result.Path, []byte(edited), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	note.Summary = "An updated summary from a newer run."
	note.ObsidianRelativePath, _ = exporter.RelativePath(result.Path)
	if _, err := exporter.Export(note); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	updated, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	document := string(updated)
	for _, preserved := range []string{"my_rating: 5", `my_project: "thesis"`, "My own thoughts about this paper."} {
		if !strings.Contains(document, preserved) {
			t.Errorf("re-export lost user content %q", preserved)
		}
	}
	if !strings.Contains(document, "An updated summary from a newer run.") {
		t.Error("re-export did not apply the new summary")
	}
}

// TestMalformedManagedNoteIsANonDestructiveConflict covers the refusal to
// overwrite a file whose markers Herald cannot trust.
func TestMalformedManagedNoteIsANonDestructiveConflict(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	result, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Duplicate the start marker so ownership is ambiguous.
	original, _ := os.ReadFile(result.Path)
	tampered := strings.Replace(string(original), FrontmatterStart,
		FrontmatterStart+"\n"+FrontmatterStart, 1)
	os.WriteFile(result.Path, []byte(tampered), 0o644)

	note.ObsidianRelativePath, _ = exporter.RelativePath(result.Path)
	if _, err := exporter.Export(note); err == nil {
		t.Fatal("Export overwrote a note with ambiguous markers")
	}
	after, _ := os.ReadFile(result.Path)
	if string(after) != tampered {
		t.Error("a conflicted note was modified anyway")
	}
}

// TestLegacyNoteMigratesAndKeepsUserFields covers upgrading a note written
// before the managed markers existed.
func TestLegacyNoteMigratesAndKeepsUserFields(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	destination := filepath.Join(exporter.VaultPath, "Herald", "Papers", "doi 10.1145 example.md")
	os.MkdirAll(filepath.Dir(destination), 0o755)
	legacy := "---\nherald_id: 7\ntitle: \"Old Title\"\nstatus: \"kept\"\n" +
		"my_rating: 4\ntags:\n  - herald\n  - old\n---\n\n" +
		"# Old Title\n\nOld body.\n\n## My Notes\n\nMy annotations survive.\n"
	os.WriteFile(destination, []byte(legacy), 0o644)

	note.ObsidianRelativePath = "Herald/Papers/doi 10.1145 example.md"
	result, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !result.MigratedLegacy {
		t.Error("the note was not reported as a legacy migration")
	}
	document, _ := os.ReadFile(result.Path)
	text := string(document)
	if !strings.Contains(text, FrontmatterStart) {
		t.Error("migration did not add the managed markers")
	}
	if !strings.Contains(text, "my_rating: 4") {
		t.Error("migration dropped a user frontmatter property")
	}
	if !strings.Contains(text, "My annotations survive.") {
		t.Error("migration dropped the user's notes")
	}
	// Herald's own legacy properties are replaced, not duplicated.
	if strings.Count(text, "herald_id:") != 1 {
		t.Errorf("herald_id appears %d times", strings.Count(text, "herald_id:"))
	}
}

// TestLegacyNoteForAnotherEntryIsRefused covers the ownership check.
func TestLegacyNoteForAnotherEntryIsRefused(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	destination := filepath.Join(exporter.VaultPath, "Herald", "Papers", "doi 10.1145 example.md")
	os.MkdirAll(filepath.Dir(destination), 0o755)
	for _, legacy := range []string{
		"---\nherald_id: 999\ntitle: \"Other\"\n---\n\n## My Notes\n\nx\n",
		"---\ntitle: \"No id\"\n---\n\n## My Notes\n\nx\n",
		"---\nherald_id: 7\n---\n\nNo notes section here.\n",
		"Not a note at all.\n",
	} {
		os.WriteFile(destination, []byte(legacy), 0o644)
		note.ObsidianRelativePath = "Herald/Papers/doi 10.1145 example.md"
		if _, err := exporter.Export(note); err == nil {
			t.Errorf("Export overwrote a file it does not own: %q", legacy)
		}
		after, _ := os.ReadFile(destination)
		if string(after) != legacy {
			t.Errorf("a refused file was modified: %q", legacy)
		}
	}
}

// TestUnkeepArchivesAndRekeepRestoresAnnotations covers the archive lifecycle.
func TestUnkeepArchivesAndRekeepRestoresAnnotations(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	result, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	relative, _ := exporter.RelativePath(result.Path)
	annotated := strings.TrimRight(mustRead(t, result.Path), "\n") + "\nAnnotation to preserve.\n"
	os.WriteFile(result.Path, []byte(annotated), 0o644)

	archive, err := exporter.Archive(note, relative)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if archive == nil {
		t.Fatal("Archive reported nothing to move")
	}
	if _, err := os.Stat(result.Path); !os.IsNotExist(err) {
		t.Error("the note is still in the vault after archiving")
	}
	// The archive lives outside the vault so Obsidian does not index it.
	if strings.HasPrefix(archive.ArchivePath, exporter.VaultPath) {
		t.Error("the archived note was written inside the vault")
	}

	archiveRelative, err := exporter.ArchiveRelativePath(archive.ArchivePath)
	if err != nil {
		t.Fatalf("ArchiveRelativePath: %v", err)
	}
	restored, err := exporter.Restore(note, archiveRelative)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !strings.Contains(mustRead(t, restored), "Annotation to preserve.") {
		t.Error("restoring lost the user's annotation")
	}

	// Restoring over an existing file must never clobber it.
	if _, err := exporter.Restore(note, archiveRelative); err == nil {
		t.Error("Restore overwrote an existing note")
	}
}

// TestPathTraversalAndSymlinksAreRejected covers the containment rules. The
// recorded note path is data and may have been edited by hand.
func TestPathTraversalAndSymlinksAreRejected(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()

	// A recorded path outside the stable layout is ignored, not followed.
	note.ObsidianRelativePath = "../../escaped.md"
	destination, err := exporter.Destination(note)
	if err != nil {
		t.Fatalf("Destination: %v", err)
	}
	if !strings.HasPrefix(destination, exporter.VaultPath) {
		t.Errorf("destination %q escaped the vault", destination)
	}

	// A symlinked directory inside the vault is refused rather than followed.
	outside := t.TempDir()
	linkParent := filepath.Join(exporter.VaultPath, "Herald")
	os.MkdirAll(linkParent, 0o755)
	if err := os.Symlink(outside, filepath.Join(linkParent, "Papers")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	note.ObsidianRelativePath = ""
	if _, err := exporter.Export(note); err == nil {
		t.Error("Export followed a symlink out of the vault")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Error("Export wrote through the symlink")
	}
}

// TestNewsUsesAPublisherSpecificPath covers the documented news layout.
func TestNewsUsesAPublisherSpecificPath(t *testing.T) {
	exporter := newExporter(t)
	note := keptNote()
	note.ContentKind = "news"
	note.SourceTitle = "Official/Company: Newsroom"
	note.CanonicalKey = "url:https://e.org/a"

	result, err := exporter.Export(note)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	relative, _ := exporter.RelativePath(result.Path)
	if !strings.HasPrefix(relative, "Herald/News/") {
		t.Errorf("news note path is %q", relative)
	}
	// Unsafe characters in the publisher name never reach the filesystem.
	if strings.Contains(relative, ":") || strings.Count(relative, "/") != 3 {
		t.Errorf("publisher directory was not sanitized: %q", relative)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(document)
}

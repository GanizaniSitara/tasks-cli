package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func testStore(t *testing.T) (*Store, taskIndex) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tasks")
	store := &Store{
		config: Config{TasksRoot: root, IndexDir: filepath.Join(root, ".index"), DefaultPrefix: "OP"},
		prefix: prefixConfig{
			Prefixes:        map[string]string{"PROJ": "Projects"},
			TwoLetterLegacy: map[string]string{"OP": "Operations"},
		},
	}
	return store, taskIndex{dir: store.config.IndexDir, manifestPath: filepath.Join(store.config.IndexDir, "manifest.json")}
}

func TestWriteParseMoveAndCompanion(t *testing.T) {
	store, _ := testStore(t)
	if err := store.ensureStructure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.config.TasksRoot, "backlog", "PROJ-001-index-task.md")
	task := &Task{
		ID: "PROJ-001", Prefix: "PROJ", Project: "PROJ", Number: 1,
		Title: "Index task", Status: "backlog", Priority: "P2", Created: "2026-07-30", Updated: "2026-07-30",
		Tags: []string{"go", "search"}, Path: path, CompanionDir: stringsTrimSuffix(path, ".md"), Frontmatter: map[string]interface{}{}, Body: "Bleve should index companion text.",
	}
	if err := os.MkdirAll(task.CompanionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.CompanionDir, "NOTE.txt"), []byte("companion keyword"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.writeTask(task); err != nil {
		t.Fatal(err)
	}
	parsed, err := store.parseTask(path, "backlog")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "PROJ-001" || parsed.Title != "Index task" || len(parsed.AssetPaths) != 1 {
		t.Fatalf("unexpected parsed task: %#v", parsed)
	}
	if err := moveTask(store, parsed, "done", "error"); err != nil {
		t.Fatal(err)
	}
	if parsed.Status != "done" {
		t.Fatalf("status = %q, want done", parsed.Status)
	}
	if _, err := os.Stat(parsed.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parsed.CompanionDir); err != nil {
		t.Fatal(err)
	}
}

func TestBleveSearchUsesSyncedIndex(t *testing.T) {
	store, idx := testStore(t)
	if err := store.ensureStructure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.config.TasksRoot, "backlog", "OP-001-fast-search.md")
	task := &Task{ID: "OP-001", Prefix: "OP", Project: "OP", Number: 1, Title: "Fast search", Status: "backlog", Path: path, Frontmatter: map[string]interface{}{}, Body: "Find the unique first keyword."}
	if err := store.writeTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.sync(store); err != nil {
		t.Fatal(err)
	}
	results, _, err := idx.search(store, "unique", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "OP-001" {
		t.Fatalf("unexpected initial search: %#v", results)
	}
	task.Body = "Find the unique second keyword."
	if err := store.writeTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.sync(store); err != nil {
		t.Fatal(err)
	}
	results, _, err = idx.search(store, "second", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "OP-001" {
		t.Fatalf("unexpected refreshed search: %#v", results)
	}
}

// A multi-term query must require every term. Bleve's MatchQuery ORs its terms
// by default, so "scraper quantumfoobar" used to return every scraper task on
// the strength of one word -- confident-looking results for a query that
// matches nothing.
func TestSearchRequiresEveryTerm(t *testing.T) {
	store, idx := testStore(t)
	if err := store.ensureStructure(); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ id, title, body string }{
		{"OP-001", "Publisher repair", "Fix the scraper publishing flow."},
		{"OP-002", "Searcher reliability", "The scraper searcher runs on remote-host."},
	} {
		path := filepath.Join(store.config.TasksRoot, "backlog", spec.id+"-fixture.md")
		task := &Task{ID: spec.id, Prefix: "OP", Project: "OP", Title: spec.title, Status: "backlog", Path: path, Frontmatter: map[string]interface{}{}, Body: spec.body}
		if err := store.writeTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := idx.sync(store); err != nil {
		t.Fatal(err)
	}

	// Both terms present in exactly one task -- and they span title and body,
	// so a per-field AND would wrongly miss it.
	results, meta, err := idx.search(store, "scraper remote-host", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "OP-002" {
		t.Fatalf("expected only OP-002, got %#v", results)
	}
	if meta["matched"] != "bleve" {
		t.Fatalf("matched = %v, want bleve", meta["matched"])
	}

	// One real term, one that matches nothing: strict finds nothing, so the
	// relaxed fallback answers -- and must say that it did.
	results, meta, err = idx.search(store, "scraper quantumfoobar", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected the relaxed fallback to return both, got %#v", results)
	}
	if meta["matched"] != "bleve-relaxed" {
		t.Fatalf("matched = %v, want bleve-relaxed", meta["matched"])
	}

	// task_id is a keyword field, indexed verbatim, so a lowercased ID must
	// still find its task rather than ranking other tickets above it.
	results, _, err = idx.search(store, "op-002", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].ID != "OP-002" {
		t.Fatalf("lowercased id should find OP-002 first, got %#v", results)
	}

	// Nothing matches at all: no results, not a page of nearest misses.
	results, _, err = idx.search(store, "quantumfoobar wombatnonsense", "", "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for a nonsense query, got %#v", results)
	}
}

func TestPrefixAllowlist(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.validatePrefix("PROJ"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.validatePrefix("OP"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.validatePrefix("XY"); err == nil {
		t.Fatal("expected unapproved two-letter prefix to fail")
	}
	if _, err := store.validatePrefix("NEW"); err == nil {
		t.Fatal("expected unapproved prefix to fail")
	}
}

func stringsTrimSuffix(value, suffix string) string {
	return value[:len(value)-len(suffix)]
}

// sandboxRun points every path at a temp tree, including its own prefix
// allowlist, and returns a runner for the real dispatch. Tests must not read the
// deploying machine's allowlist or corpus, so they behave the same on CI.
func sandboxRun(t *testing.T) func(args ...string) error {
	t.Helper()
	sandbox := t.TempDir()
	allowlist := filepath.Join(sandbox, "allowed_prefixes.yaml")
	if err := os.WriteFile(allowlist, []byte("two_letter_legacy:\n  OP: Operations\nprefixes:\n  PROJ: Projects\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(sandbox, "config.yaml")
	body := "allowed_prefixes_file: " + strconv.Quote(allowlist) + "\ndefault_prefix: OP\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASKS_CONFIG", config)
	t.Setenv("TASKS_ROOT", filepath.Join(sandbox, "tasks"))
	t.Setenv("TASKS_INDEX_DIR", filepath.Join(sandbox, "bleve"))
	return func(args ...string) error { return run(args) }
}

func TestCreateAllocatesSequentialIDs(t *testing.T) {
	tasks := sandboxRun(t)
	for i := 0; i < 3; i++ {
		if err := tasks("create", "--title", "Sequential task", "--prefix", "OP"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	root := os.Getenv("TASKS_ROOT")
	for _, id := range []string{"OP-001", "OP-002", "OP-003"} {
		matches, err := filepath.Glob(filepath.Join(root, "backlog", id+"-*.md"))
		if err != nil || len(matches) != 1 {
			t.Errorf("expected exactly one file for %s, got %v (err %v)", id, matches, err)
		}
	}
}

func TestCreateRejectsUnapprovedPrefixAndMissingTitle(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Nope", "--prefix", "ZZZ"); err == nil {
		t.Error("expected unapproved prefix to be rejected")
	}
	if err := tasks("create", "--prefix", "OP"); err == nil {
		t.Error("expected missing --title to be rejected")
	}
}

// Proves the sandbox config is in force: the loaded allowlist must be exactly
// what sandboxRun wrote, not whatever the deploying machine has installed.
func TestSandboxAllowlistReplacesMachineConfig(t *testing.T) {
	sandboxRun(t)
	store, err := newStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.prefix.Prefixes) != 1 || store.prefix.Prefixes["PROJ"] == "" {
		t.Errorf("prefixes = %v, want only PROJ", store.prefix.Prefixes)
	}
	if len(store.prefix.TwoLetterLegacy) != 1 || store.prefix.TwoLetterLegacy["OP"] == "" {
		t.Errorf("legacy = %v, want only OP", store.prefix.TwoLetterLegacy)
	}
}

func TestDeleteRequiresMatchingConfirm(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Doomed task", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("delete", "OP-001"); err == nil {
		t.Error("expected delete without --confirm to fail")
	}
	if err := tasks("delete", "OP-001", "--confirm", "OP-002"); err == nil {
		t.Error("expected delete with mismatched --confirm to fail")
	}
	if err := tasks("get", "OP-001"); err != nil {
		t.Fatalf("task should still exist after refused deletes: %v", err)
	}
	if err := tasks("delete", "OP-001", "--confirm", "OP-001"); err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	if err := tasks("get", "OP-001"); err == nil {
		t.Error("expected task to be gone after confirmed delete")
	}
}

func TestGetExactPathDisambiguatesDuplicateIDs(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "First copy", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	first := filepath.Join(root, "backlog", "OP-001-first-copy.md")
	second := filepath.Join(root, "blocked", "OP-001-second-copy.md")
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("---\ntask: OP-001\nstatus: blocked\ntitle: Second copy\n---\n\nsecond body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("get", "OP-001"); err == nil {
		t.Fatal("duplicate ID without --path must remain ambiguous")
	}
	if err := tasks("get", "OP-001", "--path", first); err != nil {
		t.Fatalf("get first exact path: %v", err)
	}
	if err := tasks("get", "OP-001", "--path", second); err != nil {
		t.Fatalf("get second exact path: %v", err)
	}
	if err := tasks("get", "OP-001", "--path", filepath.Join(root, "backlog", "missing.md")); err == nil {
		t.Fatal("unknown exact path must fail")
	}
}

func TestMoveExactPathDisambiguatesDuplicateIDs(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "First copy", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	first := filepath.Join(root, "backlog", "OP-001-first-copy.md")
	second := filepath.Join(root, "blocked", "OP-001-second-copy.md")
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("---\ntask: OP-001\nstatus: blocked\ntitle: Second copy\n---\n\nsecond body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("move", "OP-001", "done"); err == nil {
		t.Fatal("duplicate ID without --path must remain ambiguous")
	}
	if err := tasks("move", "OP-001", "done", "--path", first); err != nil {
		t.Fatalf("move first exact path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "done", filepath.Base(first))); err != nil {
		t.Fatalf("moved first copy: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("unrelated duplicate changed: %v", err)
	}
}

func TestAttachCopiesFileIntoCompanionDir(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Needs evidence", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "evidence.txt")
	if err := os.WriteFile(source, []byte("verification output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("attach", "OP-001", source); err != nil {
		t.Fatalf("attach: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-*", "evidence.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("attachment not in companion dir: %v (err %v)", matches, err)
	}
	if err := tasks("attach", "OP-001"); err == nil {
		t.Error("expected attach without a file argument to fail")
	}
	if err := tasks("attach", "OP-001", source); err == nil {
		t.Error("expected attach onto an existing asset name to fail")
	}
}

// Companion assets have to be revisable: a document attached with a wrong fact
// in it was previously uncorrectable through any sanctioned path.
func TestAssetUpdateReplacesExistingAsset(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Needs revision", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(source, []byte("original, with a wrong number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("asset", "update", "OP-001", source); err == nil {
		t.Error("expected asset update to fail when the asset does not exist yet")
	}
	if err := tasks("asset", "add", "OP-001", source); err != nil {
		t.Fatalf("asset add: %v", err)
	}
	if err := tasks("asset", "add", "OP-001", source); err == nil {
		t.Error("expected asset add to fail when the name already exists")
	}
	if err := os.WriteFile(source, []byte("corrected"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("asset", "update", "OP-001", source); err != nil {
		t.Fatalf("asset update: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-*", "brief.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("asset not in companion dir: %v (err %v)", matches, err)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "corrected" {
		t.Errorf("asset was not replaced, got %q", got)
	}
}

func TestAssetRemoveAndList(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Has assets", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(source, []byte("findings"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("asset", "add", "OP-001", source); err != nil {
		t.Fatalf("asset add: %v", err)
	}
	if err := tasks("asset", "list", "OP-001"); err != nil {
		t.Fatalf("asset list: %v", err)
	}
	if err := tasks("asset", "remove", "OP-001", "report.txt"); err != nil {
		t.Fatalf("asset remove: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-*", "report.txt"))
	if err != nil || len(matches) != 0 {
		t.Errorf("expected asset to be gone, got %v (err %v)", matches, err)
	}
	if err := tasks("asset", "remove", "OP-001", "report.txt"); err == nil {
		t.Error("expected removing a missing asset to fail")
	}
	if err := tasks("asset", "list", "OP-001"); err != nil {
		t.Errorf("asset list on an empty companion dir should succeed: %v", err)
	}
}

// An asset name must never escape the companion directory.
func TestAssetRemoveRejectsPathTraversal(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Guarded", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("asset", "remove", "OP-001", "../../escape.md"); err == nil {
		t.Error("expected a traversing asset name to be rejected")
	}
}

func TestReopenOnlyFromDone(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Round trip", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("reopen", "OP-001"); err == nil {
		t.Error("expected reopen of a backlog task to fail")
	}
	if err := tasks("move", "OP-001", "done"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("reopen", "OP-001"); err != nil {
		t.Fatalf("reopen from done: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("reopened task not in backlog: %v (err %v)", matches, err)
	}
}

func TestMigrateIsDryRunByDefault(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Original title", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("update", "OP-001", "--title", "Renamed title"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	original := filepath.Join(root, "backlog", "OP-001-original-title.md")
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("update should not rename the file: %v", err)
	}
	if err := tasks("migrate"); err != nil {
		t.Fatalf("migrate dry run: %v", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("dry-run migrate must not rename: %v", err)
	}
	if err := tasks("migrate", "--apply"); err != nil {
		t.Fatalf("migrate --apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "backlog", "OP-001-renamed-title.md")); err != nil {
		t.Fatalf("migrate --apply should rename to the new title: %v", err)
	}
}

// Every dispatchable command must have a help entry, and vice versa, so the
// advertised "tasks-cli <command> --help" can never point at a missing block.
func TestCommandHelpCoversEveryCommand(t *testing.T) {
	commands := []string{
		"summary", "projects", "search", "get", "create", "update", "move",
		"reopen", "delete", "duplicates", "note", "attach", "asset", "lint", "pivot",
		"repair", "migrate", "index",
	}
	for _, command := range commands {
		if _, ok := commandHelp[command]; !ok {
			t.Errorf("no help entry for %q", command)
		}
	}
	if len(commandHelp) != len(commands) {
		t.Errorf("commandHelp has %d entries, dispatch has %d", len(commandHelp), len(commands))
	}
}

// Help must not require a readable config, and must not be confused by a flag
// value that happens to look like a help request.
func TestHelpRequestParsing(t *testing.T) {
	// Point every path at a temp tree: this test must never touch a real corpus.
	sandbox := t.TempDir()
	t.Setenv("TASKS_CONFIG", filepath.Join(sandbox, "does-not-exist.yaml"))
	t.Setenv("TASKS_ROOT", filepath.Join(sandbox, "tasks"))
	t.Setenv("TASKS_INDEX_DIR", filepath.Join(sandbox, "bleve"))
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"create", "--help"}, {"help", "create"}, {"index", "-h"}} {
		if err := run(args); err != nil {
			t.Errorf("run(%v) = %v, want nil", args, err)
		}
	}
	if err := run([]string{"help", "nosuchcommand"}); err == nil {
		t.Error("expected unknown command error")
	}
	// "-h" as a note value must reach the note command, not print help.
	if err := run([]string{"note", "PROJ-001", "--note", "-h"}); err == nil {
		t.Error("expected note command to run and fail on a missing task")
	}
}

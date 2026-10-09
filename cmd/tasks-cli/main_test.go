package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func bulkInput(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBulkAddAllocatesAcrossPrefixesAndIndexes(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Existing", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	// Frontmatter and filename claims must both be respected across statuses.
	if err := os.WriteFile(filepath.Join(root, "done", "OP-005-mismatch.md"), []byte("---\ntask: OP-003\ntitle: Mismatch\nstatus: done\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := bulkInput(t, `[
		{"title":"First batch task","description":"uniquebulkkeyword","priority":"P2","tags":["go","cli"]},
		{"title":"Project task","project":"proj","status":"working"},
		{"title":"Second batch task","prefix":"op"}
	]`)
	result := captureRun(t, tasks, "bulk-add", "--file", path)
	if result["count"] != float64(3) || result["dry_run"] != false || result["sync"] == nil {
		t.Fatalf("unexpected bulk output: %v", result)
	}
	rows := result["tasks"].([]interface{})
	for i, id := range []string{"OP-006", "PROJ-001", "OP-007"} {
		row := rows[i].(map[string]interface{})
		if row["task_id"] != id {
			t.Errorf("row %d ID = %v, want %s", i, row["task_id"], id)
		}
		if info, err := os.Stat(row["companion_dir"].(string)); err != nil || !info.IsDir() {
			t.Fatalf("missing companion directory for %s: %v", id, err)
		}
	}
	if rows[1].(map[string]interface{})["status"] != "in-progress" {
		t.Error("status alias not normalized")
	}
	search := captureRun(t, tasks, "search", "uniquebulkkeyword")
	if ids := nextIDs(t, search); len(ids) != 1 || ids[0] != "OP-006" {
		t.Fatalf("bulk task absent from index: %v", ids)
	}
	if err := tasks("create", "--title", "After batch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "backlog", "OP-008-after-batch.md")); err != nil {
		t.Fatalf("single create did not continue batch numbering: %v", err)
	}
}

func TestBulkAddRejectsWholeInvalidBatch(t *testing.T) {
	for _, body := range []string{
		`[{"title":"Valid"},{"title":" "}]`,
		`[{"title":"Valid"},{"title":"Bad prefix","prefix":"ZZZ"}]`,
		`[{"title":"Valid"},{"title":"Bad status","status":"missing"}]`,
		`[{"title":"Valid","typo":"oops"}]`,
		`[{"title":"Valid","tags":"go"}]`,
		`[{"title":"Valid"},null]`,
		`[]`, `null`, `{}`, `[`, `[{"title":"Valid"}] []`,
	} {
		t.Run(body, func(t *testing.T) {
			tasks := sandboxRun(t)
			if err := tasks("bulk-add", "--file", bulkInput(t, body)); err == nil {
				t.Fatal("invalid batch accepted")
			}
			if _, err := os.Stat(os.Getenv("TASKS_ROOT")); !os.IsNotExist(err) {
				t.Fatalf("validation failure changed corpus: %v", err)
			}
		})
	}
}

func TestBulkAddDryRunAndStdin(t *testing.T) {
	tasks := sandboxRun(t)
	path := bulkInput(t, `[{"title":"Preview"},{"title":"Second"}]`)
	preview := captureRun(t, tasks, "bulk-add", "--file", path, "--dry-run")
	if preview["dry_run"] != true || preview["sync"] != nil {
		t.Fatalf("unexpected preview: %v", preview)
	}
	if matches, _ := filepath.Glob(filepath.Join(os.Getenv("TASKS_ROOT"), "*", "*.md")); len(matches) != 0 {
		t.Fatalf("dry run wrote tasks: %v", matches)
	}
	if _, err := os.Stat(os.Getenv("TASKS_INDEX_DIR")); !os.IsNotExist(err) {
		t.Fatalf("dry run created index: %v", err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	original := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = original })
	actual := captureRun(t, tasks, "bulk-add", "--file", "-")
	for i, row := range actual["tasks"].([]interface{}) {
		if row.(map[string]interface{})["task_id"] != preview["tasks"].([]interface{})[i].(map[string]interface{})["task_id"] {
			t.Fatal("dry run consumed task IDs")
		}
	}
}

func TestBulkAddRollsBackOnWriteFailure(t *testing.T) {
	tasks := sandboxRun(t)
	store, err := newStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureStructure(); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	// An orphan companion prevents the second creation; keep it intact.
	orphan := filepath.Join(root, "backlog", "OP-002-second")
	if err := os.Mkdir(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tasks("bulk-add", "--file", bulkInput(t, `[{"title":"First"},{"title":"Second"}]`)); err == nil {
		t.Fatal("expected companion collision to fail")
	}
	for _, path := range []string{"OP-001-first.md", "OP-001-first"} {
		if _, err := os.Stat(filepath.Join(root, "backlog", path)); !os.IsNotExist(err) {
			t.Fatalf("partial creation left behind: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("existing companion removed: %v", err)
	}
}

// A file whose frontmatter id disagrees with its filename still owns the
// number its name claims. Allocating from the frontmatter alone hands that
// number out again, producing two files with the same name in different status
// directories -- a collision `duplicates` cannot see, because the two ids
// differ.
func TestCreateSkipsNumbersClaimedByFilenames(t *testing.T) {
	tasks := sandboxRun(t)
	root := os.Getenv("TASKS_ROOT")
	if err := os.MkdirAll(filepath.Join(root, "done"), 0o755); err != nil {
		t.Fatal(err)
	}
	mismatched := filepath.Join(root, "done", "OP-003-mismatched.md")
	if err := os.WriteFile(mismatched, []byte("---\ntask: OP-002\nstatus: done\ntitle: Mismatched\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks("create", "--title", "New ticket", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "*", "OP-003-*.md")); len(matches) != 1 {
		t.Errorf("OP-003 claimed twice: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(root, "backlog", "OP-004-new-ticket.md")); err != nil {
		t.Errorf("expected the new ticket at OP-004: %v", err)
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

func TestDedupeRenumbersNewerFile(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Original ticket", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TASKS_ROOT")
	original := filepath.Join(root, "backlog", "OP-001-original-ticket.md")
	// A second writer (the feedback path on the other replication host) minted
	// the same id with a later created date.
	usurper := filepath.Join(root, "done", "OP-001-usurper-ticket.md")
	if err := os.MkdirAll(filepath.Dir(usurper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usurper, []byte("---\ntask: OP-001\nstatus: done\ntitle: Usurper\ncreated: \"2099-01-01\"\n---\n\nusurper body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Legacy numeric pair: filename 010 carrying frontmatter id 8.
	legacyKeeper := filepath.Join(root, "done", "008-legacy-keeper.md")
	legacyLoser := filepath.Join(root, "done", "010-legacy-loser.md")
	for path, id := range map[string]string{legacyKeeper: "8", legacyLoser: "8"} {
		if err := os.WriteFile(path, []byte("---\ntask: "+id+"\nstatus: done\ntitle: Legacy\n---\n\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Dry run changes nothing.
	if err := tasks("dedupe"); err != nil {
		t.Fatalf("dedupe dry run: %v", err)
	}
	if _, err := os.Stat(usurper); err != nil {
		t.Fatalf("dry run must not rename: %v", err)
	}

	if err := tasks("dedupe", "--apply"); err != nil {
		t.Fatalf("dedupe apply: %v", err)
	}
	// The original keeps its id; the usurper is renumbered to the next free OP number.
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("keeper renamed: %v", err)
	}
	renumbered := filepath.Join(root, "done", "OP-002-usurper-ticket.md")
	raw, err := os.ReadFile(renumbered)
	if err != nil {
		t.Fatalf("renumbered usurper missing: %v", err)
	}
	if !strings.Contains(string(raw), "task: OP-002") {
		t.Errorf("usurper frontmatter not renumbered: %s", raw)
	}
	if !strings.Contains(string(raw), "Renumbered from OP-001") {
		t.Errorf("no provenance note: %s", raw)
	}
	// The legacy loser reclaims the number its own filename always claimed.
	rawLegacy, err := os.ReadFile(legacyLoser)
	if err != nil {
		t.Fatalf("legacy loser moved unexpectedly: %v", err)
	}
	if !strings.Contains(string(rawLegacy), "task: \"10\"") && !strings.Contains(string(rawLegacy), "task: 10") {
		t.Errorf("legacy loser should carry id 10: %s", rawLegacy)
	}
	// And the corpus reports clean.
	if err := tasks("dedupe"); err != nil {
		t.Fatalf("dedupe recheck: %v", err)
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

func TestWontDoIsSeparateFromDone(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Dropped idea", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("move", "OP-001", "wontfix"); err != nil {
		t.Fatalf("move to wont-do via alias: %v", err)
	}
	root := os.Getenv("TASKS_ROOT")
	matches, err := filepath.Glob(filepath.Join(root, "wont-do", "OP-001-*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("task not in wont-do: %v (err %v)", matches, err)
	}
	if done, _ := filepath.Glob(filepath.Join(root, "done", "OP-001-*.md")); len(done) != 0 {
		t.Fatalf("wont-do task leaked into done: %v", done)
	}
	if err := tasks("reopen", "OP-001"); err != nil {
		t.Fatalf("reopen from wont-do: %v", err)
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

// captureRun runs the dispatch with stdout redirected and decodes the JSON, so
// a command can be asserted on what it actually reports rather than only on the
// files it leaves behind.
func captureRun(t *testing.T, runner func(args ...string) error, args ...string) map[string]interface{} {
	t.Helper()
	real := os.Stdout
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writePipe
	runErr := runner(args...)
	writePipe.Close()
	os.Stdout = real
	raw, err := io.ReadAll(readPipe)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("run(%v): %v", args, runErr)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %v output %q: %v", args, string(raw), err)
	}
	return decoded
}

func nextIDs(t *testing.T, result map[string]interface{}) []string {
	t.Helper()
	rows, _ := result["results"].([]interface{})
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		item, _ := row.(map[string]interface{})
		ids = append(ids, item["task_id"].(string))
	}
	return ids
}

// pinToday freezes the clock seam so a deferral horizon does not depend on when
// the suite runs.
func pinToday(t *testing.T, value string) {
	t.Helper()
	original := today
	today = func() string { return value }
	t.Cleanup(func() { today = original })
}

// The whole point of next: a deferred, declined, waiting-on-someone or
// wrong-context task is not work you can pick up, and must not lead the list.
func TestNextSuppressesUnactionableWork(t *testing.T) {
	pinToday(t, "2026-09-12")
	tasks := sandboxRun(t)
	for _, title := range []string{"Ready now", "Deferred", "Declined", "Waiting", "On the Mac"} {
		if err := tasks("create", "--title", title, "--prefix", "OP", "--priority", "P2"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tasks("defer", "OP-002", "--until", "2026-09-26"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("decline", "OP-003", "--for", "14d"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("update", "OP-004", "--waiting-on", "sellers"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("update", "OP-005", "--context", "mac"); err != nil {
		t.Fatal(err)
	}

	result := captureRun(t, tasks, "next", "--context", "desktop")
	if got := nextIDs(t, result); len(got) != 1 || got[0] != "OP-001" {
		t.Fatalf("next --context desktop = %v, want [OP-001]", got)
	}
	suppressed, _ := result["suppressed"].(map[string]interface{})
	for _, key := range []string{"deferred", "declined", "waiting", "wrong_context"} {
		if suppressed[key] != float64(1) {
			t.Errorf("suppressed[%q] = %v, want 1", key, suppressed[key])
		}
	}

	// --all must bring every one of them back, or the hiding is lossy.
	if got := nextIDs(t, captureRun(t, tasks, "next", "--all")); len(got) != 5 {
		t.Errorf("next --all returned %d tasks, want 5", len(got))
	}
	// A context nobody claimed still sees the unlabelled work.
	if got := nextIDs(t, captureRun(t, tasks, "next", "--context", "mac")); len(got) != 2 {
		t.Errorf("next --context mac = %v, want OP-001 and OP-005", got)
	}
}

// A deferral expires by itself. Without this the field is just a nicer-looking
// version of leaving the task in blocked forever.
func TestNextReturnsTaskAfterDeferralLapses(t *testing.T) {
	pinToday(t, "2026-09-12")
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Comes back", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("defer", "OP-001", "--for", "2w"); err != nil {
		t.Fatal(err)
	}
	if got := nextIDs(t, captureRun(t, tasks, "next")); len(got) != 0 {
		t.Fatalf("deferred task still offered: %v", got)
	}
	pinToday(t, "2026-09-27")
	if got := nextIDs(t, captureRun(t, tasks, "next")); len(got) != 1 {
		t.Fatalf("task did not return after its defer date: %v", got)
	}
}

// Priority still wins; staleness only breaks ties. This is the rotation that
// stops the same head leading the list every day.
func TestNextRanksStalestFirstWithinAPriority(t *testing.T) {
	pinToday(t, "2026-09-12")
	store, _ := testStore(t)
	if err := store.ensureStructure(); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, priority, updated string }{
		{"OP-001", "P2", "2026-09-11"},
		{"OP-002", "P2", "2026-01-04"},
		{"OP-003", "P1", "2026-09-12"},
	} {
		task := &Task{
			ID: row.id, Prefix: "OP", Title: row.id, Status: "backlog", Priority: row.priority,
			Updated: row.updated, Path: filepath.Join(store.config.TasksRoot, "backlog", row.id+"-t.md"),
		}
		if err := store.writeTask(task); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TASKS_ROOT", store.config.TasksRoot)
	t.Setenv("TASKS_INDEX_DIR", store.config.IndexDir)
	t.Setenv("TASKS_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	result := captureRun(t, func(args ...string) error { return run(args) }, "next")
	got := nextIDs(t, result)
	want := []string{"OP-003", "OP-002", "OP-001"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("next order = %v, want %v (P1 first, then stalest)", got, want)
	}
}

// The reason must outlive the session that decided it, so it goes in the body
// and not only into a frontmatter date.
func TestDeferRecordsReasonAndClears(t *testing.T) {
	pinToday(t, "2026-09-12")
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Park me", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("defer", "OP-001", "--until", "2026-09-26", "--because", "waiting on seller replies"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-park-me.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "defer_until: \"2026-09-26\"") && !strings.Contains(string(raw), "defer_until: 2026-09-26") {
		t.Errorf("defer_until not written:\n%s", raw)
	}
	if !strings.Contains(string(raw), "waiting on seller replies") {
		t.Errorf("reason not recorded in the body:\n%s", raw)
	}
	if err := tasks("defer", "OP-001", "--clear"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "defer_until:") {
		t.Errorf("--clear left the field behind:\n%s", raw)
	}
}

// Defer without a horizon is the mistake that turns a park into a disappearance.
func TestDeferRequiresAHorizonButDeclineDefaults(t *testing.T) {
	pinToday(t, "2026-09-12")
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Needs a date", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	if err := tasks("defer", "OP-001"); err == nil {
		t.Error("defer with no --until or --for should fail")
	}
	result := captureRun(t, tasks, "decline", "OP-001")
	if result["declined_until"] != "2026-09-26" {
		t.Errorf("decline default = %v, want 2026-09-26 (14 days)", result["declined_until"])
	}
}

func TestBackfillIsDryRunByDefault(t *testing.T) {
	tasks := sandboxRun(t)
	if err := tasks("create", "--title", "Join-key inventory (AT WORK)", "--prefix", "OP"); err != nil {
		t.Fatal(err)
	}
	result := captureRun(t, tasks, "backfill")
	if result["count"] != float64(1) {
		t.Fatalf("backfill should have found one task, got %v", result["count"])
	}
	path := filepath.Join(os.Getenv("TASKS_ROOT"), "backlog", "OP-001-join-key-inventory-at-work.md")
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "context:") {
		t.Errorf("dry run must not write:\n%s", raw)
	}
	if err := tasks("backfill", "--apply"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "terminal") {
		t.Errorf("--apply should have set context: terminal:\n%s", raw)
	}
}

func TestParseForRejectsNonsense(t *testing.T) {
	for _, good := range []struct {
		value string
		days  int
	}{{"14d", 14}, {"2w", 14}, {"1m", 30}} {
		if got, err := parseFor(good.value); err != nil || got != good.days {
			t.Errorf("parseFor(%q) = %d, %v; want %d", good.value, got, err, good.days)
		}
	}
	for _, bad := range []string{"", "14", "d", "0d", "-3d", "2y", "later"} {
		if _, err := parseFor(bad); err == nil {
			t.Errorf("parseFor(%q) should have failed", bad)
		}
	}
}

// A malformed date must not hide a task. Silent disappearance is worse than a
// stale one appearing.
func TestPendingIgnoresUnparseableDates(t *testing.T) {
	for _, value := range []string{"", "soon", "2026-13-99", "next tuesday"} {
		if pending(value, "2026-09-12") {
			t.Errorf("pending(%q) = true, want false", value)
		}
	}
	if !pending("2026-09-26", "2026-09-12") {
		t.Error("a future date should be pending")
	}
	if pending("2026-09-12", "2026-09-12") {
		t.Error("a task due today should be actionable, not pending")
	}
}

// Every dispatchable command must have a help entry, and vice versa, so the
// advertised "tasks-cli <command> --help" can never point at a missing block.
func TestCommandHelpCoversEveryCommand(t *testing.T) {
	commands := []string{
		"summary", "projects", "search", "get", "create", "bulk-add", "update", "move",
		"reopen", "delete", "duplicates", "dedupe", "note", "attach", "asset",
		"lint", "pivot", "repair", "migrate", "index", "version",
		"next", "defer", "decline", "backfill",
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
	// version answers before the store opens, for the same reason help does:
	// identifying a binary must work on a machine whose config is missing.
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"create", "--help"}, {"help", "create"}, {"index", "-h"}, {"version"}, {"--version"}} {
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

func TestSearchSynonymsRecencyAndLimits(t *testing.T) {
	tasks := sandboxRun(t)

	if err := tasks("create", "--title", "Harvest web sessions", "--prefix", "OP", "--description", "Harvesting session transcripts nightly."); err != nil {
		t.Fatal(err)
	}
	if err := tasks("create", "--title", "Old scraper task", "--prefix", "OP", "--description", "An old scraper task from long ago."); err != nil {
		t.Fatal(err)
	}

	// 1. Synonym search: searching "scrape" should match "Harvest web sessions" via synonym expansion
	search := captureRun(t, tasks, "search", "scrape")
	ids := nextIDs(t, search)
	foundHarvest := false
	for _, id := range ids {
		if id == "OP-001" {
			foundHarvest = true
			break
		}
	}
	if !foundHarvest {
		t.Fatalf("expected search 'scrape' to match OP-001 via synonym expansion, got ids: %v", ids)
	}
	if total, ok := search["total"].(float64); !ok || total < 2 {
		t.Fatalf("expected total >= 2 in search output, got %v", search["total"])
	}

	// 2. Disabling synonyms with --no-synonyms should NOT match OP-001 for 'scrape'
	searchNoSyn := captureRun(t, tasks, "search", "scrape", "--no-synonyms")
	idsNoSyn := nextIDs(t, searchNoSyn)
	for _, id := range idsNoSyn {
		if id == "OP-001" {
			t.Fatalf("expected OP-001 NOT to match with --no-synonyms, got: %v", idsNoSyn)
		}
	}

	// 3. Multi-word synonym query: "scrape history" should match OP-001 ("Harvest ... transcripts")
	searchMulti := captureRun(t, tasks, "search", "scrape history")
	idsMulti := nextIDs(t, searchMulti)
	foundMulti := false
	for _, id := range idsMulti {
		if id == "OP-001" {
			foundMulti = true
			break
		}
	}
	if !foundMulti {
		t.Fatalf("expected 'scrape history' to match OP-001 via multi-word synonym expansion, got: %v", idsMulti)
	}

	// 4. Test --limit and --all
	searchLimited := captureRun(t, tasks, "search", "harvest", "--limit", "1")
	if len(nextIDs(t, searchLimited)) != 1 {
		t.Fatalf("expected limit 1 to return 1 item, got: %v", len(nextIDs(t, searchLimited)))
	}
	if total, ok := searchLimited["total"].(float64); !ok || total < 2 {
		t.Fatalf("expected total count to report true total >= 2, got: %v", searchLimited["total"])
	}

	searchAll := captureRun(t, tasks, "search", "harvest", "--all")
	if len(nextIDs(t, searchAll)) < 2 {
		t.Fatalf("expected --all to return all matching tasks, got: %v", len(nextIDs(t, searchAll)))
	}

	// 5. Test --sort recency
	searchRecent := captureRun(t, tasks, "search", "--sort", "recency", "--brief")
	recentIDs := nextIDs(t, searchRecent)
	if len(recentIDs) < 2 {
		t.Fatalf("expected at least 2 items, got: %v", recentIDs)
	}
}

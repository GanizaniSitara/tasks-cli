package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blevesearch/bleve/v2"
	blevequery "github.com/blevesearch/bleve/v2/search/query"
	"go.yaml.in/yaml/v3"
)

var statuses = []string{"backlog", "in-progress", "blocked", "done"}

var statusAliases = map[string]string{
	"todo": "backlog", "queued": "backlog", "backlog": "backlog",
	"in_progress": "in-progress", "in-progress": "in-progress", "progress": "in-progress", "working": "in-progress",
	"blocked": "blocked", "block": "blocked", "stalled": "blocked",
	"done": "done", "complete": "done", "completed": "done", "closed": "done",
}

var readableAssetSuffixes = map[string]bool{
	".css": true, ".csv": true, ".html": true, ".js": true, ".json": true,
	".md": true, ".ps1": true, ".py": true, ".sql": true, ".toml": true,
	".ts": true, ".tsv": true, ".txt": true, ".yaml": true, ".yml": true,
}

var canonicalID = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]+)-(\d+)$`)
var dottedID = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]+)-([0-9.]+)$`)
var wordPrefixID = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]+)[_-]`)
var canonicalStem = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]+-[0-9]+(?:\.[0-9]+)*)[_-](.+)$`)
var numericStem = regexp.MustCompile(`^(\d+)[_-](.+)$`)

type Config struct {
	TasksRoot           string `yaml:"tasks_root"`
	IndexDir            string `yaml:"index_dir"`
	AllowedPrefixesFile string `yaml:"allowed_prefixes_file"`
	DefaultPrefix       string `yaml:"default_prefix"`
}

type prefixConfig struct {
	TwoLetterLegacy map[string]string `yaml:"two_letter_legacy"`
	Prefixes        map[string]string `yaml:"prefixes"`
}

type Task struct {
	ID           string                 `json:"task_id"`
	Prefix       string                 `json:"prefix,omitempty"`
	Project      string                 `json:"project,omitempty"`
	Number       int                    `json:"number,omitempty"`
	Title        string                 `json:"title"`
	Status       string                 `json:"status"`
	Priority     string                 `json:"priority,omitempty"`
	Created      string                 `json:"created,omitempty"`
	Updated      string                 `json:"updated,omitempty"`
	Tags         []string               `json:"tags"`
	Path         string                 `json:"path"`
	CompanionDir string                 `json:"companion_dir,omitempty"`
	Frontmatter  map[string]interface{} `json:"frontmatter"`
	AssetPaths   []string               `json:"asset_paths"`
	Body         string                 `json:"body,omitempty"`
	AssetBlob    string                 `json:"asset_blob,omitempty"`
	Slug         string                 `json:"-"`
	Signature    string                 `json:"-"`
}

type manifest struct {
	Entries map[string]manifestEntry `json:"entries"`
}

type manifestEntry struct {
	Signature string `json:"signature"`
}

type Store struct {
	config     Config
	prefix     prefixConfig
	configPath string
}

type taskIndex struct {
	dir          string
	manifestPath string
}

type optionalString struct {
	value string
	set   bool
}

func (o *optionalString) String() string { return o.value }
func (o *optionalString) Set(value string) error {
	o.value = value
	o.set = true
	return nil
}

type multiValue []string

func (m *multiValue) String() string { return strings.Join(*m, ",") }
func (m *multiValue) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		emitError(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || (args[0] == "help" && len(args) == 1) {
		printHelp()
		return nil
	}
	// "tasks-cli help create" and "tasks-cli create --help" both resolve command help.
	// Only the leading position is treated as a help request, so a flag value
	// that happens to be "-h" or "--help" is still passed through as data.
	if args[0] == "help" {
		return printCommandHelp(args[1])
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		return printCommandHelp(args[0])
	}
	store, err := newStore()
	if err != nil {
		return err
	}
	index := taskIndex{dir: store.config.IndexDir, manifestPath: filepath.Join(store.config.IndexDir, "manifest.json")}

	switch args[0] {
	case "summary":
		return commandSummary(store, index)
	case "projects":
		emit(map[string]interface{}{"projects": store.projects(), "prefixes": sortedKeys(store.prefix.Prefixes), "two_letter_legacy": sortedKeys(store.prefix.TwoLetterLegacy)})
		return nil
	case "search":
		return commandSearch(store, index, args[1:])
	case "get":
		return commandGet(store, args[1:])
	case "create":
		return commandCreate(store, index, args[1:])
	case "update":
		return commandUpdate(store, index, args[1:])
	case "move":
		return commandMove(store, index, args[1:])
	case "reopen":
		return commandReopen(store, index, args[1:])
	case "delete":
		return commandDelete(store, index, args[1:])
	case "duplicates":
		return commandDuplicates(store)
	case "note":
		return commandNote(store, index, args[1:])
	case "attach":
		return commandAttach(store, index, args[1:])
	case "lint":
		return commandLint(store, index)
	case "pivot":
		return commandPivot(store, args[1:])
	case "repair":
		return commandRepair(store, index, args[1:])
	case "migrate":
		return commandMigrate(store, index, args[1:])
	case "asset":
		return commandAsset(store, index, args[1:])
	case "index":
		return commandIndex(store, index, args[1:])
	default:
		return fmt.Errorf("unknown command %q; run tasks-cli help", args[0])
	}
}

func printHelp() {
	fmt.Println(`tasks-cli <command> [flags]

Commands:
  summary | projects | search | get | create | update | move | reopen | delete
  duplicates | note | attach | lint | pivot | repair | migrate
  index sync | index rebuild

All successful commands emit JSON. Markdown files remain the source of truth.
Flags may appear before or after positional arguments.

Run "tasks-cli <command> --help" (or "tasks-cli help <command>") for command flags.`)
	printConfigSummary()
}

// printConfigSummary reports where this installation is actually reading and
// writing, so an agent can discover the layout by probing rather than being
// told. Resolution can fail on a fresh machine -- help must still work there,
// so a failure degrades to naming the overrides instead of erroring.
func printConfigSummary() {
	fmt.Println("\nConfiguration (TASKS_CONFIG, TASKS_ROOT, TASKS_INDEX_DIR override):")
	store, err := newStore()
	if err != nil {
		fmt.Printf("  unresolved: %v\n", err)
		return
	}
	for _, row := range [][2]string{
		{"config", store.configPath},
		{"corpus", store.config.TasksRoot},
		{"index", store.config.IndexDir},
		{"prefixes", store.config.AllowedPrefixesFile},
		{"default prefix", store.config.DefaultPrefix},
	} {
		if row[1] != "" {
			fmt.Printf("  %-15s %s\n", row[0], row[1])
		}
	}
}

// commandHelp holds one usage block per command. Flag descriptions live here
// rather than in each FlagSet because every FlagSet discards its own output,
// so the flag package never prints them.
var commandHelp = map[string]string{
	"summary": `tasks-cli summary

Counts per status, per-prefix totals, and the index location. No flags.`,

	"projects": `tasks-cli projects

Allowed project prefixes from the configured allowlist. No flags.`,

	"duplicates": `tasks-cli duplicates

Task IDs that appear in more than one file. No flags.`,

	"lint": `tasks-cli lint

Corpus integrity report: duplicate IDs, missing prefixes, orphan companion
directories, non-normalized priorities, and status mismatches. No flags.`,

	"search": `tasks-cli search [QUERY] [flags]

Search the Bleve index. With no QUERY, lists tasks from disk instead, which is
how you enumerate a status (there is no separate "list" command).

  --status STATUS      backlog | in-progress | blocked | done
  --prefix PREFIX      restrict to one project prefix, e.g. PROJ
  --project PROJECT    restrict to one project field
  --limit N            maximum results (default 20)
  --include-content    include companion-file text in each result

Every term must match, though each may match in any field (id, title, tags,
body). If nothing matches all of them the search retries with any-term
matching rather than returning nothing, and reports which pass answered in
sync.matched:

  bleve          strict -- every term is present in every result
  bleve-relaxed  fallback -- results share only some terms, so treat them as
                 candidates and check each one before relying on it

  tasks-cli search "wine scraper" --limit 5
  tasks-cli search --status in-progress --prefix PROJ`,

	"get": `tasks-cli get TASK-ID [--path EXACT-PATH]

One task with its frontmatter, body, and companion asset paths.

Flags:
  --path EXACT-PATH   disambiguate when the ID resolves to several files`,

	"create": `tasks-cli create --title TEXT [flags]

Create a task and its companion directory. The prefix must already be in the
allowlist; the command will not invent a new project code.

  --title TEXT             required
  --description TEXT       body text
  --description-file PATH  read body from a file (mutually exclusive)
  --prefix PREFIX          project prefix, e.g. PROJ
  --project PROJECT        project field (defaults to the prefix)
  --status STATUS          default backlog
  --priority P0..P5
  --tag TAG                repeat the flag for multiple tags

  tasks-cli create --title "Index companion files" --prefix PROJ --tag go --tag cli`,

	"update": `tasks-cli update TASK-ID [flags]

Update metadata or body. Only the flags you pass are changed. Renaming the
title does not rename the file; run "tasks-cli migrate" to reconcile file stems.

  --title TEXT
  --description TEXT
  --description-file PATH
  --priority P0..P5
  --project PROJECT
  --status STATUS          moves the file if the status changes
  --tag TAG                repeat the flag for multiple tags
  --clear-tags             drop all existing tags

  tasks-cli update PROJ-092 --priority P1 --tag go`,

	"move": `tasks-cli move TASK-ID STATUS [flags]

Move a task and its companion directory to another status directory.

	--path EXACT-PATH                  disambiguate when the ID resolves to several files
	--strategy error|replace|merge   companion-directory collision handling
	                                 (default error)

  tasks-cli move PROJ-092 done`,

	"reopen": `tasks-cli reopen TASK-ID [flags]

Move a done task back into an active status and append a History note.

  --status STATUS   backlog | in-progress | blocked (default backlog)`,

	"delete": `tasks-cli delete TASK-ID --confirm TASK-ID [flags]

Delete a task file and its companion directory. Deliberately explicit: the
--confirm value must repeat the task ID.

  --confirm TASK-ID   required, must match
  --path EXACT-PATH   disambiguate when the ID resolves to several files`,

	"note": `tasks-cli note TASK-ID --note TEXT [flags]

Append a timestamped note under a heading, creating the heading if absent.

  --note TEXT         note body
  --note-file PATH    read the note from a file
  --heading NAME      target heading (default Notes)`,

	"attach": `tasks-cli attach TASK-ID FILE

Copy FILE into the task's companion directory and index its text. Alias for
"tasks-cli asset add"; fails if an asset of that name already exists. Use
"tasks-cli asset update" to replace one.

  tasks-cli attach PROJ-092 C:\reports\verification.txt`,

	"asset": `tasks-cli asset add|update|remove|list TASK-ID [FILE|NAME]

Manage the files in a task's companion directory. Assets are matched by
basename.

  add     TASK-ID FILE   copy FILE in; fails if that name already exists
  update  TASK-ID FILE   replace the asset of the same name; fails if absent
  remove  TASK-ID NAME   delete the named asset
  list    TASK-ID        name, size and sha256 for every asset

"update" and "remove" report previous_size and previous_sha256 so a change is
auditable from the output alone. Every mutation re-indexes the companion text.

  tasks-cli asset update PROJ-092 C:\reports\verification.txt
  tasks-cli asset remove PROJ-092 stale-notes.md`,

	"pivot": `tasks-cli pivot [flags]

Cross-tabulate the corpus.

  --rows AXIS         project | prefix | status | priority (default project)
  --cols AXIS         project | prefix | status | priority (default status)
  --status LIST       comma-separated statuses to include
  --priorities LIST   comma-separated priorities to include
  --projects LIST     comma-separated projects to include`,

	"repair": `tasks-cli repair --fix FIX [flags]

Dry-run unless --apply is given. Requires at least one --fix.

  --fix NORMALIZE_PRIORITY        rewrite priorities to canonical P0..P5
  --fix RESOLVE_STATUS_MISMATCH   align frontmatter status with the directory
  --fix FLAG_SPLIT_BRAIN          report IDs living in two statuses
  --fix REBUILD_BLEVE             rebuild the search index
                                  (REBUILD_WHOOSH is a legacy alias for this)
  --apply                         perform the changes

  tasks-cli repair --fix NORMALIZE_PRIORITY
  tasks-cli repair --fix NORMALIZE_PRIORITY --apply`,

	"migrate": `tasks-cli migrate [flags]

Reconcile legacy filenames and file stems with canonical task IDs and titles.
Dry-run unless --apply is given.

  --apply   perform the renames`,

	"index": `tasks-cli index sync|rebuild

  sync      index only tasks whose files changed since the last run
  rebuild   discard the index and rebuild it from every markdown file

Mutations update the index automatically. Run "sync" when a non-CLI writer has
changed the corpus.`,
}

func printCommandHelp(command string) error {
	text, ok := commandHelp[command]
	if !ok {
		return fmt.Errorf("unknown command %q; run tasks-cli help", command)
	}
	fmt.Println(text)
	return nil
}

func newStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(home, ".config")
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = filepath.Join(home, ".local", "share")
	}
	configPath := os.Getenv("TASKS_CONFIG")
	if configPath == "" {
		configPath = filepath.Join(appData, "tasks-cli", "config.yaml")
	}
	config := Config{
		TasksRoot:           filepath.Join(home, "tasks"),
		IndexDir:            filepath.Join(localAppData, "tasks-cli", "bleve"),
		AllowedPrefixesFile: filepath.Join(appData, "tasks-cli", "allowed_prefixes.yaml"),
		DefaultPrefix:       "OP",
	}
	if raw, readErr := os.ReadFile(configPath); readErr == nil {
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return nil, fmt.Errorf("read %s: %w", configPath, err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if override := os.Getenv("TASKS_ROOT"); override != "" {
		config.TasksRoot = override
	}
	if override := os.Getenv("TASKS_INDEX_DIR"); override != "" {
		config.IndexDir = override
	}
	config.TasksRoot, _ = filepath.Abs(config.TasksRoot)
	config.IndexDir, _ = filepath.Abs(config.IndexDir)
	config.AllowedPrefixesFile, _ = filepath.Abs(config.AllowedPrefixesFile)
	config.DefaultPrefix = strings.ToUpper(strings.TrimSpace(config.DefaultPrefix))

	prefix := prefixConfig{Prefixes: map[string]string{}, TwoLetterLegacy: map[string]string{}}
	raw, err := os.ReadFile(config.AllowedPrefixesFile)
	if err != nil {
		return nil, fmt.Errorf("read prefix allowlist %s: %w", config.AllowedPrefixesFile, err)
	}
	if err := yaml.Unmarshal(raw, &prefix); err != nil {
		return nil, fmt.Errorf("read prefix allowlist: %w", err)
	}
	if prefix.Prefixes == nil {
		prefix.Prefixes = map[string]string{}
	}
	if prefix.TwoLetterLegacy == nil {
		prefix.TwoLetterLegacy = map[string]string{}
	}
	for key, value := range prefix.Prefixes {
		delete(prefix.Prefixes, key)
		prefix.Prefixes[strings.ToUpper(key)] = value
	}
	for key, value := range prefix.TwoLetterLegacy {
		delete(prefix.TwoLetterLegacy, key)
		prefix.TwoLetterLegacy[strings.ToUpper(key)] = value
	}
	return &Store{config: config, prefix: prefix, configPath: configPath}, nil
}

func (s *Store) projects() []string {
	all := map[string]bool{}
	for key := range s.prefix.Prefixes {
		all[key] = true
	}
	for key := range s.prefix.TwoLetterLegacy {
		all[key] = true
	}
	return sortedBoolKeys(all)
}

func (s *Store) validatePrefix(value string) (string, error) {
	prefix := strings.ToUpper(strings.TrimSpace(value))
	if !regexp.MustCompile(`^[A-Z][A-Z0-9]+$`).MatchString(prefix) {
		return "", fmt.Errorf("invalid prefix %q: use uppercase letters/digits, starting with a letter, minimum 2 characters", value)
	}
	if len(prefix) == 2 {
		if _, ok := s.prefix.TwoLetterLegacy[prefix]; ok {
			return prefix, nil
		}
		return "", fmt.Errorf("prefix %q is not on the frozen two-letter allowlist", prefix)
	}
	if _, ok := s.prefix.Prefixes[prefix]; ok {
		return prefix, nil
	}
	return "", fmt.Errorf("prefix %q is not in the allowlist; ask the user before adding a new code", prefix)
}

func (s *Store) ensureStructure() error {
	for _, status := range statuses {
		if err := os.MkdirAll(filepath.Join(s.config.TasksRoot, status), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStatus(value string) (string, error) {
	status, ok := statusAliases[strings.ToLower(strings.TrimSpace(value))]
	if !ok {
		return "", fmt.Errorf("unknown status %q", value)
	}
	return status, nil
}

func (s *Store) scanTasks() ([]*Task, error) {
	if err := s.ensureStructure(); err != nil {
		return nil, err
	}
	var tasks []*Task
	for _, status := range statuses {
		dir := filepath.Join(s.config.TasksRoot, status)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" || strings.EqualFold(entry.Name(), "README.md") {
				continue
			}
			task, err := s.parseTask(filepath.Join(dir, entry.Name()), status)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return strings.ToUpper(tasks[i].ID) < strings.ToUpper(tasks[j].ID) })
	return tasks, nil
}

// inventory records only filesystem metadata. It deliberately avoids reading
// every task and attachment on each search; changed entries are parsed later.
func (s *Store) inventory() (map[string]string, error) {
	if err := s.ensureStructure(); err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, status := range statuses {
		dir := filepath.Join(s.config.TasksRoot, status)
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || strings.ToLower(filepath.Ext(file.Name())) != ".md" || strings.EqualFold(file.Name(), "README.md") {
				continue
			}
			path := filepath.Join(dir, file.Name())
			signature, err := quickSignature(path)
			if err != nil {
				return nil, err
			}
			entries[path] = signature
		}
	}
	return entries, nil
}

func quickSignature(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	parts := []string{path, strconv.FormatInt(info.Size(), 10), strconv.FormatInt(info.ModTime().UnixNano(), 10)}
	companion := strings.TrimSuffix(path, filepath.Ext(path))
	if _, err := os.Stat(companion); errors.Is(err, os.ErrNotExist) {
		return signatureFor(parts), nil
	} else if err != nil {
		return "", err
	}
	err = filepath.WalkDir(companion, func(asset string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		assetInfo, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(companion, asset)
		if err != nil {
			return err
		}
		parts = append(parts, filepath.ToSlash(rel), strconv.FormatInt(assetInfo.Size(), 10), strconv.FormatInt(assetInfo.ModTime().UnixNano(), 10))
		return nil
	})
	if err != nil {
		return "", err
	}
	return signatureFor(parts), nil
}

func (s *Store) parseTask(path, status string) (*Task, error) {
	return s.parseTaskWithAssets(path, status, true)
}

func (s *Store) parseTaskLite(path, status string) (*Task, error) {
	return s.parseTaskWithAssets(path, status, false)
}

func (s *Store) parseTaskWithAssets(path, status string, includeAssetContent bool) (*Task, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, body := splitFrontmatter(string(raw))
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	id, slug := splitStem(stem)
	for _, key := range []string{"task", "ticket", "id"} {
		if value := stringValue(meta[key]); value != "" {
			id = value
			break
		}
	}
	prefix, number := parseID(id)
	project := strings.ToUpper(stringValue(meta["project"]))
	if project == "" {
		project = prefix
	}
	title := stringValue(meta["title"])
	if title == "" {
		title = humanize(slug)
	}
	companion := strings.TrimSuffix(path, filepath.Ext(path))
	assetBlob, assetPaths, sigParts := "", []string(nil), []string(nil)
	if includeAssetContent {
		assetBlob, assetPaths, sigParts, err = loadAssets(companion)
		if err != nil {
			return nil, err
		}
	} else {
		assetPaths, err = assetPathsOnly(companion)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	signature := signatureFor(append([]string{path, strconv.FormatInt(info.Size(), 10), strconv.FormatInt(info.ModTime().UnixNano(), 10)}, sigParts...))
	tags := normalizeTags(meta["tags"])
	return &Task{
		ID: id, Prefix: prefix, Project: project, Number: number, Title: title, Status: status,
		Priority: stringValue(meta["priority"]), Created: stringValue(meta["created"]), Updated: stringValue(meta["updated"]),
		Tags: tags, Path: path, CompanionDir: companionIfExists(companion), Frontmatter: meta, AssetPaths: assetPaths,
		Body: body, AssetBlob: assetBlob, Slug: slug, Signature: signature,
	}, nil
}

func splitFrontmatter(raw string) (map[string]interface{}, string) {
	meta := map[string]interface{}{}
	if !strings.HasPrefix(raw, "---\n") && !strings.HasPrefix(raw, "---\r\n") {
		return meta, strings.TrimLeft(raw, "\r\n")
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return meta, strings.TrimLeft(raw, "\r\n")
	}
	front := strings.Join(lines[1:end], "\n")
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		for _, line := range strings.Split(front, "\n") {
			if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) != "" {
				meta[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return meta, strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\r\n")
}

func splitStem(stem string) (string, string) {
	if match := canonicalStem.FindStringSubmatch(stem); len(match) == 3 {
		return match[1], slugify(match[2])
	}
	if match := numericStem.FindStringSubmatch(stem); len(match) == 3 {
		return match[1], slugify(match[2])
	}
	// Historical TASK-style files use the full underscore stem as their ID
	// (for example TASK_fix-session-naming), not merely the TASK prefix.
	if before, after, ok := strings.Cut(stem, "_"); ok && before != "" && after != "" {
		return stem, slugify(after)
	}
	if match := regexp.MustCompile(`^(.+?)[_-](.+)$`).FindStringSubmatch(stem); len(match) == 3 {
		return match[1], slugify(match[2])
	}
	return stem, ""
}

func parseID(value string) (string, int) {
	if match := canonicalID.FindStringSubmatch(strings.TrimSpace(value)); len(match) == 3 {
		number, _ := strconv.Atoi(match[2])
		return strings.ToUpper(match[1]), number
	}
	if match := dottedID.FindStringSubmatch(strings.TrimSpace(value)); len(match) == 3 {
		number, _ := strconv.Atoi(strings.ReplaceAll(match[2], ".", ""))
		return strings.ToUpper(match[1]), number
	}
	if match := wordPrefixID.FindStringSubmatch(strings.TrimSpace(value)); len(match) == 2 {
		return strings.ToUpper(match[1]), 0
	}
	return "", 0
}

func canonicalTaskID(prefix string, number int) string {
	width := 3
	if len(strconv.Itoa(number)) > width {
		width = len(strconv.Itoa(number))
	}
	return fmt.Sprintf("%s-%0*d", strings.ToUpper(prefix), width, number)
}

func companionIfExists(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return ""
}

func loadAssets(dir string) (string, []string, []string, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	if !info.IsDir() {
		return "", nil, nil, nil
	}
	var chunks, paths, sigParts []string
	chars := 0
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		sigParts = append(sigParts, filepath.ToSlash(rel), strconv.FormatInt(fileInfo.Size(), 10), strconv.FormatInt(fileInfo.ModTime().UnixNano(), 10))
		if !readableAssetSuffixes[strings.ToLower(filepath.Ext(path))] || fileInfo.Size() > 256*1024 {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		block := "\n\n### " + filepath.ToSlash(rel) + "\n\n" + strings.TrimSpace(string(content))
		if strings.TrimSpace(block) == "" || chars+len(block) > 250000 {
			return nil
		}
		chunks = append(chunks, block)
		chars += len(block)
		return nil
	})
	if err != nil {
		return "", nil, nil, err
	}
	sort.Strings(paths)
	sort.Strings(sigParts)
	return strings.TrimSpace(strings.Join(chunks, "")), paths, sigParts, nil
}

func assetPathsOnly(dir string) ([]string, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var paths []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func signatureFor(parts []string) string {
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
func normalizeTags(value interface{}) []string {
	var raw []string
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			raw = append(raw, stringValue(item))
		}
	case []string:
		raw = append(raw, typed...)
	case string:
		raw = strings.Split(typed, ",")
	default:
		if value != nil {
			raw = []string{stringValue(value)}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	hyphen := false
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			hyphen = false
		} else if !hyphen && builder.Len() > 0 {
			builder.WriteByte('-')
			hyphen = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func humanize(slug string) string {
	slug = strings.ReplaceAll(strings.ReplaceAll(slug, "-", " "), "_", " ")
	if slug == "" {
		return "Untitled task"
	}
	return strings.ToUpper(slug[:1]) + slug[1:]
}
func sortedKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
func sortedBoolKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (s *Store) find(taskID string) (*Task, error) {
	return s.findAtPath(taskID, "")
}

func (s *Store) findAtPath(taskID, exactPath string) (*Task, error) {
	tasks, err := s.scanTasks()
	if err != nil {
		return nil, err
	}
	needle := strings.ToUpper(strings.TrimSpace(taskID))
	var matches []*Task
	for _, task := range tasks {
		if strings.ToUpper(task.ID) == needle && (exactPath == "" || samePath(task.Path, exactPath)) {
			matches = append(matches, task)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("task %q has %d copies; use duplicates and an exact path", taskID, len(matches))
	}
	return matches[0], nil
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil && rightErr == nil {
		left, right = leftAbs, rightAbs
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func writeAtomic(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tasks-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.WriteString(text); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tempName, path)
}

func (s *Store) writeTask(task *Task) error {
	if task.Frontmatter == nil {
		task.Frontmatter = map[string]interface{}{}
	}
	delete(task.Frontmatter, "ticket")
	delete(task.Frontmatter, "id")
	task.Frontmatter["task"] = task.ID
	task.Frontmatter["title"] = task.Title
	task.Frontmatter["status"] = task.Status
	if task.Project != "" {
		task.Frontmatter["project"] = task.Project
	}
	if task.Priority != "" {
		task.Frontmatter["priority"] = task.Priority
	} else {
		delete(task.Frontmatter, "priority")
	}
	if task.Created != "" {
		task.Frontmatter["created"] = task.Created
	}
	if task.Updated != "" {
		task.Frontmatter["updated"] = task.Updated
	}
	if len(task.Tags) > 0 {
		task.Frontmatter["tags"] = task.Tags
	} else {
		delete(task.Frontmatter, "tags")
	}
	encoded, err := yaml.Marshal(task.Frontmatter)
	if err != nil {
		return err
	}
	text := "---\n" + string(encoded) + "---\n"
	if strings.TrimSpace(task.Body) != "" {
		text += "\n" + strings.TrimSpace(task.Body) + "\n"
	}
	return writeAtomic(task.Path, text)
}

// processAlive reports whether a pid currently exists. Both hosts that run this
// binary are covered: on Windows os.FindProcess opens a handle and fails when
// the process is gone, while on Unix it always succeeds and liveness has to be
// probed with signal 0. An EPERM there means the process exists but belongs to
// somebody else, which still counts as alive.
//
// Every uncertain answer is "alive", so the caller waits rather than reclaims.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return errors.Is(err, os.ErrPermission)
	}
	return true
}

// lockOwnerDead reports whether the pid recorded in a lock file is definitely
// gone. It is deliberately conservative: anything it cannot establish — missing
// file, unparseable pid, a live process, or an error it does not understand —
// returns false and leaves the lock alone. Wrongly reclaiming a held lock lets
// two writers into the corpus at once; wrongly waiting only costs time, and the
// age fallback still clears it eventually.
func lockOwnerDead(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid := 0
	for _, line := range strings.Split(string(data), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "pid="); found {
			if parsed, convErr := strconv.Atoi(rest); convErr == nil {
				pid = parsed
			}
			break
		}
	}
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	return !processAlive(pid)
}

func withLock(root string, fn func() error) error {
	path := filepath.Join(root, ".tasks-cli.lock")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// The lock is released by the deferred Remove below, so any abnormal exit
		// — killed process, panic, closed pipe — orphans it. Two independent
		// staleness tests, cheapest first.
		//
		// Liveness: the lock records the owning pid, so ask whether that process
		// still exists. A dead owner means the lock cannot be released by anyone
		// and is reclaimable immediately, however recently it was written.
		if lockOwnerDead(path) {
			_ = os.Remove(path)
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		}
		// Age: the fallback for when the pid is unreadable or has been recycled
		// onto an unrelated process. Deliberately long, because reclaiming a lock
		// a live writer still holds is far worse than waiting.
		if err != nil {
			if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 30*time.Minute {
				_ = os.Remove(path)
				file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			}
		}
		if err != nil {
			return fmt.Errorf("task corpus is busy: %w", err)
		}
	}
	_, _ = file.WriteString(fmt.Sprintf("pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
	_ = file.Close()
	defer os.Remove(path)
	return fn()
}

func taskMap(task *Task, includeContent bool) map[string]interface{} {
	result := map[string]interface{}{"task_id": task.ID, "prefix": emptyNil(task.Prefix), "project": emptyNil(task.Project), "number": nil, "title": task.Title, "status": task.Status, "priority": emptyNil(task.Priority), "created": emptyNil(task.Created), "updated": emptyNil(task.Updated), "tags": nonNilStrings(task.Tags), "path": task.Path, "companion_dir": emptyNil(task.CompanionDir), "frontmatter": task.Frontmatter, "asset_paths": nonNilStrings(task.AssetPaths)}
	if task.Number > 0 {
		result["number"] = task.Number
	}
	if includeContent {
		result["body"] = task.Body
		result["asset_blob"] = task.AssetBlob
	}
	return result
}
func emptyNil(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (idx taskIndex) open() (bleve.Index, error) {
	if err := os.MkdirAll(idx.dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(idx.dir, "index_meta.json")); errors.Is(err, os.ErrNotExist) {
		mapping := bleve.NewIndexMapping()
		doc := bleve.NewDocumentMapping()
		text := bleve.NewTextFieldMapping()
		text.Store = false
		keyword := bleve.NewKeywordFieldMapping()
		keyword.Store = false
		doc.AddFieldMappingsAt("task_id", keyword)
		doc.AddFieldMappingsAt("prefix", keyword)
		doc.AddFieldMappingsAt("project", keyword)
		doc.AddFieldMappingsAt("status", keyword)
		doc.AddFieldMappingsAt("tags", text)
		doc.AddFieldMappingsAt("title", text)
		doc.AddFieldMappingsAt("content", text)
		mapping.DefaultMapping = doc
		return bleve.New(idx.dir, mapping)
	} else if err != nil {
		return nil, err
	}
	return bleve.Open(idx.dir)
}

func (idx taskIndex) loadManifest() manifest {
	result := manifest{Entries: map[string]manifestEntry{}}
	raw, err := os.ReadFile(idx.manifestPath)
	if err == nil {
		_ = json.Unmarshal(raw, &result)
	}
	if result.Entries == nil {
		result.Entries = map[string]manifestEntry{}
	}
	return result
}

func (idx taskIndex) saveManifest(value manifest) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(idx.manifestPath, string(raw))
}

func (idx taskIndex) sync(store *Store) (map[string]interface{}, error) {
	inventory, err := store.inventory()
	if err != nil {
		return nil, err
	}
	index, err := idx.open()
	if err != nil {
		return nil, err
	}
	defer index.Close()
	old := idx.loadManifest()
	next := manifest{Entries: map[string]manifestEntry{}}
	batch := index.NewBatch()
	updated, removed := 0, 0
	seen := map[string]bool{}
	for path, signature := range inventory {
		seen[path] = true
		next.Entries[path] = manifestEntry{Signature: signature}
		if previous, ok := old.Entries[path]; ok && previous.Signature == signature {
			continue
		}
		status := filepath.Base(filepath.Dir(path))
		task, err := store.parseTask(path, status)
		if err != nil {
			return nil, err
		}
		content := task.Title + "\n\n" + task.Body + "\n\n" + task.AssetBlob
		batch.Index(task.Path, map[string]interface{}{"task_id": task.ID, "prefix": task.Prefix, "project": task.Project, "status": task.Status, "tags": strings.Join(task.Tags, " "), "title": task.Title, "content": content})
		updated++
	}
	for path := range old.Entries {
		if !seen[path] {
			batch.Delete(path)
			removed++
		}
	}
	if updated > 0 || removed > 0 {
		if err := index.Batch(batch); err != nil {
			return nil, err
		}
	}
	if err := idx.saveManifest(next); err != nil {
		return nil, err
	}
	return map[string]interface{}{"scanned": len(inventory), "updated": updated, "removed": removed, "index_dir": idx.dir}, nil
}

func (idx taskIndex) rebuild(store *Store) (map[string]interface{}, error) {
	if err := os.RemoveAll(idx.dir); err != nil {
		return nil, err
	}
	return idx.sync(store)
}

func (idx taskIndex) search(store *Store, query, status, prefix, project string, limit int, includeAssetContent bool) ([]*Task, map[string]interface{}, error) {
	index, err := idx.open()
	if err != nil {
		return nil, nil, err
	}
	defer index.Close()
	query = strings.TrimSpace(query)

	// Run strictly first: every query term must appear somewhere in the task.
	// If that finds nothing, fall back to the permissive any-term search so a
	// query is never answered with silence when partial matches exist -- but
	// say so in the emitted mode, because the two mean very different things.
	matched := "bleve"
	hits, err := idx.runQuery(index, buildQuery(query, true), status, prefix, project, limit)
	if err != nil {
		return nil, nil, err
	}
	if len(hits) == 0 && query != "" {
		matched = "bleve-relaxed"
		hits, err = idx.runQuery(index, buildQuery(query, false), status, prefix, project, limit)
		if err != nil {
			return nil, nil, err
		}
	}

	var out []*Task
	for _, path := range hits {
		statusName := filepath.Base(filepath.Dir(path))
		var task *Task
		var err error
		if includeAssetContent {
			task, err = store.parseTask(path, statusName)
		} else {
			task, err = store.parseTaskLite(path, statusName)
		}
		if err == nil {
			out = append(out, task)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, map[string]interface{}{"index_dir": idx.dir, "mode": "on-write", "matched": matched}, nil
}

// buildQuery turns a query string into a Bleve query.
//
// A term is searched across every field at that field's boost, so a task
// matches when the word appears in its title OR its tags OR its body -- the
// field split must not fragment a multi-word query. When requireAll is set the
// per-term groups are conjoined, so "scraper quantumfoobar" finds nothing
// rather than returning every scraper task on the strength of one word. That
// AND default
// is what made the previous Whoosh-backed search feel precise; Bleve's
// MatchQuery ORs its terms, which is why a nonsense query still returned a full
// page of confident-looking results.
func buildQuery(query string, requireAll bool) blevequery.Query {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return bleve.NewMatchAllQuery()
	}
	groups := make([]blevequery.Query, 0, len(terms))
	for _, term := range terms {
		// task_id is a keyword field, so it is indexed verbatim and never
		// lowercased. Upper-casing the term here is what lets "proj-17" find
		// PROJ-17 instead of quietly ranking PROJ-1 and PROJ-170 above it.
		id := bleve.NewMatchQuery(strings.ToUpper(term))
		id.SetField("task_id")
		id.SetBoost(10)
		title := bleve.NewMatchQuery(term)
		title.SetField("title")
		title.SetBoost(5)
		tags := bleve.NewMatchQuery(term)
		tags.SetField("tags")
		tags.SetBoost(3)
		content := bleve.NewMatchQuery(term)
		content.SetField("content")
		groups = append(groups, bleve.NewDisjunctionQuery(id, title, tags, content))
	}
	if len(groups) == 1 {
		return groups[0]
	}
	if requireAll {
		return bleve.NewConjunctionQuery(groups...)
	}
	return bleve.NewDisjunctionQuery(groups...)
}

// runQuery applies the status/prefix/project filters and returns matching paths
// in score order.
func (idx taskIndex) runQuery(index bleve.Index, root blevequery.Query, status, prefix, project string, limit int) ([]string, error) {
	filters := []blevequery.Query{root}
	for field, value := range map[string]string{"status": status, "prefix": prefix, "project": project} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		term := bleve.NewTermQuery(strings.TrimSpace(value))
		term.SetField(field)
		filters = append(filters, term)
	}
	if len(filters) > 1 {
		root = bleve.NewConjunctionQuery(filters...)
	}
	request := bleve.NewSearchRequestOptions(root, limit*4, 0, false)
	result, err := index.Search(request)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(result.Hits))
	for _, hit := range result.Hits {
		paths = append(paths, hit.ID)
	}
	return paths, nil
}

func commandSummary(store *Store, idx taskIndex) error {
	tasks, err := store.scanTasks()
	if err != nil {
		return err
	}
	sync, err := idx.sync(store)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	prefixes := map[string]int{}
	for _, status := range statuses {
		counts[status] = 0
	}
	for _, task := range tasks {
		counts[task.Status]++
		if task.Prefix != "" {
			prefixes[task.Prefix]++
		}
	}
	emit(map[string]interface{}{"tasks_root": store.config.TasksRoot, "index": map[string]interface{}{"index_dir": idx.dir}, "counts": counts, "prefix_counts": prefixes, "missing_status_dirs": []string{}, "sync": sync})
	return nil
}

// The standard flag package stops at the first positional argument. LLMs and
// humans naturally put the task ID or query first, so accept flags anywhere.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		registered := fs.Lookup(name)
		if registered == nil {
			return fmt.Errorf("unknown flag %s", arg)
		}
		flags = append(flags, arg)
		if hasValue {
			continue
		}
		if _, ok := registered.Value.(interface{ IsBoolFlag() bool }); ok {
			continue
		}
		if i+1 >= len(args) {
			return fmt.Errorf("flag %s requires a value", arg)
		}
		i++
		flags = append(flags, args[i])
	}
	return fs.Parse(append(flags, positional...))
}

func commandSearch(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := fs.String("status", "", "")
	prefix := fs.String("prefix", "", "")
	project := fs.String("project", "", "")
	limit := fs.Int("limit", 20, "")
	include := fs.Bool("include-content", false, "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	query := strings.Join(fs.Args(), " ")
	if *limit < 1 {
		return fmt.Errorf("limit must be positive")
	}
	if *status != "" {
		var err error
		*status, err = normalizeStatus(*status)
		if err != nil {
			return err
		}
	}
	if query == "" {
		tasks, err := store.scanTasks()
		if err != nil {
			return err
		}
		var filtered []*Task
		for _, task := range tasks {
			if (*status == "" || task.Status == *status) && (*prefix == "" || strings.EqualFold(task.Prefix, *prefix)) && (*project == "" || strings.EqualFold(task.Project, *project)) {
				filtered = append(filtered, task)
			}
		}
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].Updated+filtered[i].Created > filtered[j].Updated+filtered[j].Created
		})
		if len(filtered) > *limit {
			filtered = filtered[:*limit]
		}
		emitSearch(query, *status, *prefix, *project, "filesystem", filtered, *include, nil)
		return nil
	}
	tasks, sync, err := idx.search(store, query, *status, strings.ToUpper(*prefix), strings.ToUpper(*project), *limit, *include)
	if err != nil {
		return err
	}
	emitSearch(query, *status, *prefix, *project, "bleve", tasks, *include, sync)
	return nil
}

func emitSearch(query, status, prefix, project, source string, tasks []*Task, include bool, sync map[string]interface{}) {
	results := make([]map[string]interface{}, 0, len(tasks))
	for _, task := range tasks {
		item := taskMap(task, include)
		if query != "" {
			item["snippet"] = snippet(task.Title+"\n"+task.Body+"\n"+task.AssetBlob, query)
		}
		results = append(results, item)
	}
	emit(map[string]interface{}{"query": query, "status": emptyNil(status), "prefix": emptyNil(prefix), "project": emptyNil(project), "source": source, "results": results, "sync": sync})
}

func snippet(content, query string) string {
	needle := strings.ToLower(strings.TrimSpace(query))
	lower := strings.ToLower(content)
	at := strings.Index(lower, needle)
	if at < 0 {
		if len(content) > 220 {
			return content[:220]
		}
		return content
	}
	start := at - 40
	if start < 0 {
		start = 0
	}
	end := at + len(query) + 180
	if end > len(content) {
		end = len(content)
	}
	return strings.TrimSpace(content[start:end])
}

func commandGet(store *Store, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("path", "", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return fmt.Errorf("usage: tasks-cli get TASK-ID [--path EXACT-PATH]")
	}
	task, err := store.findAtPath(fs.Args()[0], *path)
	if err != nil {
		return err
	}
	emit(taskMap(task, true))
	return nil
}

func commandCreate(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	title := fs.String("title", "", "")
	description := fs.String("description", "", "")
	descriptionFile := fs.String("description-file", "", "")
	prefix := fs.String("prefix", "", "")
	project := fs.String("project", "", "")
	status := fs.String("status", "backlog", "")
	priority := fs.String("priority", "", "")
	var tags multiValue
	fs.Var(&tags, "tag", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" {
		return fmt.Errorf("create requires --title")
	}
	if *description != "" && *descriptionFile != "" {
		return fmt.Errorf("use one of --description or --description-file")
	}
	if *descriptionFile != "" {
		value, err := os.ReadFile(*descriptionFile)
		if err != nil {
			return err
		}
		*description = string(value)
	}
	normalizedStatus, err := normalizeStatus(*status)
	if err != nil {
		return err
	}
	return withLock(store.config.TasksRoot, func() error {
		chosenPrefix := *prefix
		if chosenPrefix == "" {
			chosenPrefix = *project
		}
		if chosenPrefix == "" {
			chosenPrefix = store.config.DefaultPrefix
		}
		chosenPrefix, err = store.validatePrefix(chosenPrefix)
		if err != nil {
			return err
		}
		chosenProject := chosenPrefix
		if *project != "" {
			chosenProject = strings.ToUpper(*project)
		}
		tasks, err := store.scanTasks()
		if err != nil {
			return err
		}
		next := 1
		for _, task := range tasks {
			if strings.EqualFold(task.Prefix, chosenPrefix) && task.Number >= next {
				next = task.Number + 1
			}
		}
		id := canonicalTaskID(chosenPrefix, next)
		slug := slugify(*title)
		if slug == "" {
			slug = "untitled"
		}
		path := filepath.Join(store.config.TasksRoot, normalizedStatus, id+"-"+slug+".md")
		today := time.Now().Format("2006-01-02")
		task := &Task{ID: id, Prefix: chosenPrefix, Project: chosenProject, Number: next, Title: strings.TrimSpace(*title), Status: normalizedStatus, Priority: strings.TrimSpace(*priority), Created: today, Updated: today, Tags: splitTags(tags), Path: path, CompanionDir: strings.TrimSuffix(path, ".md"), Frontmatter: map[string]interface{}{}, Body: *description}
		if err := os.MkdirAll(task.CompanionDir, 0o755); err != nil {
			return err
		}
		if err := store.writeTask(task); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"task": taskMap(task, true), "sync": sync})
		return nil
	})
}

func splitTags(values []string) []string {
	var all []string
	for _, value := range values {
		all = append(all, strings.Split(value, ",")...)
	}
	return normalizeTags(all)
}

func commandUpdate(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var title, description, priority, project, status optionalString
	fs.Var(&title, "title", "")
	fs.Var(&description, "description", "")
	descriptionFile := fs.String("description-file", "", "")
	fs.Var(&priority, "priority", "")
	fs.Var(&project, "project", "")
	fs.Var(&status, "status", "")
	var tags multiValue
	fs.Var(&tags, "tag", "")
	clearTags := fs.Bool("clear-tags", false, "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return fmt.Errorf("usage: tasks-cli update TASK-ID [flags]")
	}
	if description.set && *descriptionFile != "" {
		return fmt.Errorf("use one of --description or --description-file")
	}
	if *descriptionFile != "" {
		raw, err := os.ReadFile(*descriptionFile)
		if err != nil {
			return err
		}
		description.value = string(raw)
		description.set = true
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.find(fs.Args()[0])
		if err != nil {
			return err
		}
		if title.set {
			task.Title = title.value
		}
		if description.set {
			task.Body = description.value
		}
		if priority.set {
			task.Priority = priority.value
		}
		if project.set {
			task.Project = strings.ToUpper(project.value)
		}
		if status.set {
			task.Status, err = normalizeStatus(status.value)
			if err != nil {
				return err
			}
			target := filepath.Join(store.config.TasksRoot, task.Status, filepath.Base(task.Path))
			if target != task.Path {
				if _, err := os.Stat(target); err == nil {
					return fmt.Errorf("destination already exists: %s", target)
				}
				if err := os.Rename(task.Path, target); err != nil {
					return err
				}
				oldCompanion := strings.TrimSuffix(task.Path, ".md")
				newCompanion := strings.TrimSuffix(target, ".md")
				if _, err := os.Stat(oldCompanion); err == nil {
					if err := os.Rename(oldCompanion, newCompanion); err != nil {
						return err
					}
				}
				task.Path = target
				task.CompanionDir = companionIfExists(newCompanion)
			}
		}
		if *clearTags {
			task.Tags = nil
		}
		if len(tags) > 0 {
			task.Tags = splitTags(tags)
		}
		task.Updated = time.Now().Format("2006-01-02")
		if err := store.writeTask(task); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"task": taskMap(task, true), "sync": sync})
		return nil
	})
}

func commandMove(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("path", "", "")
	strategy := fs.String("strategy", "error", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 2 {
		return fmt.Errorf("usage: tasks-cli move TASK-ID STATUS [--path EXACT-PATH] [--strategy error|replace|merge]")
	}
	targetStatus, err := normalizeStatus(fs.Args()[1])
	if err != nil {
		return err
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.findAtPath(fs.Args()[0], *path)
		if err != nil {
			return err
		}
		if task.Status == targetStatus {
			emit(map[string]interface{}{"task": taskMap(task, true), "sync": map[string]interface{}{}})
			return nil
		}
		if err := moveTask(store, task, targetStatus, *strategy); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"task": taskMap(task, true), "sync": sync})
		return nil
	})
}

func moveTask(store *Store, task *Task, status, strategy string) error {
	target := filepath.Join(store.config.TasksRoot, status, filepath.Base(task.Path))
	targetCompanion := strings.TrimSuffix(target, ".md")
	sourceCompanion := strings.TrimSuffix(task.Path, ".md")
	if _, err := os.Stat(target); err == nil {
		switch strategy {
		case "replace":
			_ = os.Remove(target)
			_ = os.RemoveAll(targetCompanion)
		case "merge":
			existing, err := store.parseTask(target, status)
			if err != nil {
				return err
			}
			if strings.TrimSpace(existing.Body) != "" {
				task.Body = strings.TrimSpace(task.Body) + "\n\n## Archived from previous copy\n\n" + strings.TrimSpace(existing.Body)
			}
			if err := mergeCompanions(sourceCompanion, targetCompanion); err != nil {
				return err
			}
			_ = os.Remove(target)
			_ = os.RemoveAll(targetCompanion)
		default:
			return fmt.Errorf("destination already exists: %s", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(task.Path, target); err != nil {
		return err
	}
	if _, err := os.Stat(sourceCompanion); err == nil {
		if err := os.Rename(sourceCompanion, targetCompanion); err != nil {
			return err
		}
	}
	task.Path = target
	task.CompanionDir = companionIfExists(targetCompanion)
	task.Status = status
	task.Updated = time.Now().Format("2006-01-02")
	return store.writeTask(task)
}

func mergeCompanions(source, destination string) error {
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		target := filepath.Join(source, rel)
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}

func commandReopen(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("reopen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := fs.String("status", "backlog", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return fmt.Errorf("usage: tasks-cli reopen TASK-ID [--status backlog|in-progress]")
	}
	target, err := normalizeStatus(*status)
	if err != nil {
		return err
	}
	if target != "backlog" && target != "in-progress" {
		return fmt.Errorf("reopen target must be backlog or in-progress")
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.find(fs.Args()[0])
		if err != nil {
			return err
		}
		if task.Status != "done" {
			return fmt.Errorf("only done tasks can be reopened")
		}
		task.Body = appendHeading(task.Body, "History", fmt.Sprintf("%s — reopened to %s", time.Now().Format("2006-01-02"), target))
		if err := moveTask(store, task, target, "error"); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"task": taskMap(task, true), "sync": sync})
		return nil
	})
}

func commandDelete(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("path", "", "")
	confirm := fs.String("confirm", "", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return fmt.Errorf("usage: tasks-cli delete TASK-ID --confirm TASK-ID [--path EXACT-PATH]")
	}
	id := fs.Args()[0]
	if !strings.EqualFold(id, *confirm) {
		return fmt.Errorf("delete requires --confirm %s", id)
	}
	return withLock(store.config.TasksRoot, func() error {
		tasks, err := store.scanTasks()
		if err != nil {
			return err
		}
		var matches []*Task
		for _, task := range tasks {
			if strings.EqualFold(task.ID, id) && (*path == "" || filepath.Clean(task.Path) == filepath.Clean(*path)) {
				matches = append(matches, task)
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("task %q not found", id)
		}
		if len(matches) > 1 {
			return fmt.Errorf("task %q has duplicates; supply --path", id)
		}
		task := matches[0]
		if err := os.Remove(task.Path); err != nil {
			return err
		}
		if task.CompanionDir != "" {
			if err := os.RemoveAll(task.CompanionDir); err != nil {
				return err
			}
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"deleted_task_id": task.ID, "deleted_path": task.Path, "sync": sync})
		return nil
	})
}

func commandDuplicates(store *Store) error {
	tasks, err := store.scanTasks()
	if err != nil {
		return err
	}
	byID := map[string][]string{}
	for _, task := range tasks {
		byID[strings.ToUpper(task.ID)] = append(byID[strings.ToUpper(task.ID)], task.Path)
	}
	duplicates := map[string][]string{}
	for id, paths := range byID {
		if len(paths) > 1 {
			duplicates[id] = paths
		}
	}
	emit(map[string]interface{}{"count": len(duplicates), "duplicates": duplicates})
	return nil
}

func appendHeading(body, heading, note string) string {
	marker := "## " + heading
	trimmed := strings.TrimSpace(body)
	if strings.Contains(trimmed, marker) {
		return trimmed + "\n\n" + note + "\n"
	}
	if trimmed == "" {
		return marker + "\n\n" + note + "\n"
	}
	return trimmed + "\n\n" + marker + "\n\n" + note + "\n"
}

func commandNote(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	note := fs.String("note", "", "")
	noteFile := fs.String("note-file", "", "")
	heading := fs.String("heading", "Notes", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 || (*note == "" && *noteFile == "") || (*note != "" && *noteFile != "") {
		return fmt.Errorf("usage: tasks-cli note TASK-ID --note TEXT|--note-file PATH [--heading NAME]")
	}
	if *noteFile != "" {
		raw, err := os.ReadFile(*noteFile)
		if err != nil {
			return err
		}
		*note = string(raw)
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.find(fs.Args()[0])
		if err != nil {
			return err
		}
		task.Body = appendHeading(task.Body, *heading, time.Now().Format("2006-01-02 15:04")+" — "+strings.TrimSpace(*note))
		task.Updated = time.Now().Format("2006-01-02")
		if err := store.writeTask(task); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"task": taskMap(task, true), "sync": sync})
		return nil
	})
}

// attach predates the asset command group and stays as an alias for "asset add".
func commandAttach(store *Store, idx taskIndex, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: tasks-cli attach TASK-ID FILE")
	}
	return assetWrite(store, idx, args, assetAdd)
}

func commandAsset(store *Store, idx taskIndex, args []string) error {
	usage := fmt.Errorf("usage: tasks-cli asset add|update|remove|list TASK-ID [FILE|NAME]")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "add":
		return assetWrite(store, idx, args[1:], assetAdd)
	case "update":
		return assetWrite(store, idx, args[1:], assetUpdate)
	case "remove":
		return assetRemove(store, idx, args[1:])
	case "list":
		return assetList(store, args[1:])
	default:
		return usage
	}
}

type assetMode int

const (
	assetAdd assetMode = iota
	assetUpdate
)

// companionDir is the directory holding a task's assets: the task file path
// with its .md suffix removed.
func companionDir(task *Task) string {
	return strings.TrimSuffix(task.Path, ".md")
}

// assetName rejects anything that is not a bare filename, so an asset operation
// can never escape the companion directory.
func assetName(candidate string) (string, error) {
	name := filepath.Base(candidate)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return "", fmt.Errorf("invalid asset name")
	}
	return name, nil
}

func assetWrite(store *Store, idx taskIndex, args []string, mode assetMode) error {
	verb := "add"
	if mode == assetUpdate {
		verb = "update"
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: tasks-cli asset %s TASK-ID FILE", verb)
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.find(args[0])
		if err != nil {
			return err
		}
		source := args[1]
		name, err := assetName(source)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		companion := companionDir(task)
		if err := os.MkdirAll(companion, 0o755); err != nil {
			return err
		}
		destination := filepath.Join(companion, name)
		previous, readErr := os.ReadFile(destination)
		exists := readErr == nil
		if mode == assetAdd && exists {
			return fmt.Errorf("asset already exists: %s (use \"tasks-cli asset update\" to replace it)", name)
		}
		if mode == assetUpdate && !exists {
			return fmt.Errorf("asset does not exist: %s (use \"tasks-cli asset add\" to create it)", name)
		}
		if err := os.WriteFile(destination, raw, 0o644); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		payload := map[string]interface{}{"task_id": task.ID, "saved_path": destination, "size": len(raw), "sha256": hex.EncodeToString(sum[:]), "sync": sync}
		if mode == assetUpdate {
			previousSum := sha256.Sum256(previous)
			payload["replaced"] = true
			payload["previous_size"] = len(previous)
			payload["previous_sha256"] = hex.EncodeToString(previousSum[:])
		}
		emit(payload)
		return nil
	})
}

func assetRemove(store *Store, idx taskIndex, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: tasks-cli asset remove TASK-ID NAME")
	}
	return withLock(store.config.TasksRoot, func() error {
		task, err := store.find(args[0])
		if err != nil {
			return err
		}
		name, err := assetName(args[1])
		if err != nil {
			return err
		}
		destination := filepath.Join(companionDir(task), name)
		previous, err := os.ReadFile(destination)
		if err != nil {
			return fmt.Errorf("asset does not exist: %s", name)
		}
		if err := os.Remove(destination); err != nil {
			return err
		}
		sync, err := idx.sync(store)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(previous)
		emit(map[string]interface{}{"task_id": task.ID, "removed_path": destination, "previous_size": len(previous), "previous_sha256": hex.EncodeToString(sum[:]), "sync": sync})
		return nil
	})
}

func assetList(store *Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: tasks-cli asset list TASK-ID")
	}
	task, err := store.find(args[0])
	if err != nil {
		return err
	}
	companion := companionDir(task)
	assets := []map[string]interface{}{}
	entries, err := os.ReadDir(companion)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(companion, entry.Name()))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		assets = append(assets, map[string]interface{}{"name": entry.Name(), "size": len(raw), "sha256": hex.EncodeToString(sum[:])})
	}
	emit(map[string]interface{}{"task_id": task.ID, "companion_dir": companion, "assets": assets})
	return nil
}

func commandLint(store *Store, idx taskIndex) error {
	tasks, err := store.scanTasks()
	if err != nil {
		return err
	}
	_, _ = idx.sync(store)
	byID := map[string][]*Task{}
	mismatch := []map[string]string{}
	missingPrefix := []map[string]string{}
	badPriority := []map[string]string{}
	for _, task := range tasks {
		byID[strings.ToUpper(task.ID)] = append(byID[strings.ToUpper(task.ID)], task)
		if raw := stringValue(task.Frontmatter["status"]); raw != "" && !strings.EqualFold(raw, task.Status) {
			mismatch = append(mismatch, map[string]string{"task_id": task.ID, "path": task.Path, "frontmatter_status": raw, "folder_status": task.Status})
		}
		if task.Prefix == "" {
			missingPrefix = append(missingPrefix, map[string]string{"task_id": task.ID, "path": task.Path})
		}
		if task.Priority != "" && canonicalPriority(task.Priority) != task.Priority {
			badPriority = append(badPriority, map[string]string{"task_id": task.ID, "priority": task.Priority})
		}
	}
	duplicates := map[string][]string{}
	for id, values := range byID {
		if len(values) > 1 {
			for _, task := range values {
				duplicates[id] = append(duplicates[id], task.Path)
			}
		}
	}
	orphan := []map[string]string{}
	for _, status := range statuses {
		dir := filepath.Join(store.config.TasksRoot, status)
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			stub := filepath.Join(dir, entry.Name()+".md")
			if _, err := os.Stat(stub); errors.Is(err, os.ErrNotExist) {
				orphan = append(orphan, map[string]string{"dir": filepath.Join(dir, entry.Name()), "expected_stub": stub})
			}
		}
	}
	warnings := []string{}
	if len(duplicates) > 0 {
		warnings = append(warnings, fmt.Sprintf("duplicate_task_ids: %d", len(duplicates)))
	}
	if len(mismatch) > 0 {
		warnings = append(warnings, fmt.Sprintf("status_mismatches: %d", len(mismatch)))
	}
	if len(orphan) > 0 {
		warnings = append(warnings, fmt.Sprintf("orphan_companion_dirs: %d", len(orphan)))
	}
	if len(missingPrefix) > 0 {
		warnings = append(warnings, fmt.Sprintf("missing_prefix: %d", len(missingPrefix)))
	}
	emit(map[string]interface{}{"integrity": map[string]interface{}{"duplicate_task_ids": duplicates, "status_mismatches": mismatch, "orphan_companion_dirs": orphan, "split_brain_content": []interface{}{}, "priority_non_normalized": map[string]interface{}{"count": len(badPriority), "samples": badPriority}, "missing_prefix": missingPrefix}, "counts": map[string]int{"files_on_disk": len(tasks), "unique_task_ids": len(byID), "delta": len(tasks) - len(byID)}, "warnings": warnings})
	return nil
}

func canonicalPriority(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "0", "P0":
		return "P0"
	case "1", "P1":
		return "P1"
	case "2", "P2":
		return "P2"
	case "3", "P3":
		return "P3"
	case "4", "P4":
		return "P4"
	case "5", "P5":
		return "P5"
	}
	return value
}

func commandPivot(store *Store, args []string) error {
	fs := flag.NewFlagSet("pivot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rows := fs.String("rows", "project", "")
	cols := fs.String("cols", "status", "")
	status := fs.String("status", "", "")
	priorities := fs.String("priorities", "", "")
	projects := fs.String("projects", "", "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	valid := map[string]bool{"project": true, "prefix": true, "status": true, "priority": true}
	if !valid[*rows] || !valid[*cols] {
		return fmt.Errorf("rows and cols must be project, prefix, status, or priority")
	}
	tasks, err := store.scanTasks()
	if err != nil {
		return err
	}
	statusSet := stringSet(*status)
	prioritySet := stringSet(*priorities)
	projectSet := stringSet(*projects)
	matrix := map[string]map[string]int{}
	rowValues := map[string]bool{}
	colValues := map[string]bool{}
	for _, task := range tasks {
		priority := canonicalPriority(task.Priority)
		if len(statusSet) > 0 && !statusSet[strings.ToUpper(task.Status)] {
			continue
		}
		if len(prioritySet) > 0 && !prioritySet[priority] {
			continue
		}
		if len(projectSet) > 0 && !projectSet[strings.ToUpper(task.Project)] {
			continue
		}
		row := pivotValue(task, *rows, priority)
		col := pivotValue(task, *cols, priority)
		if matrix[row] == nil {
			matrix[row] = map[string]int{}
		}
		matrix[row][col]++
		rowValues[row] = true
		colValues[col] = true
	}
	emit(map[string]interface{}{"rows": *rows, "cols": *cols, "row_values": sortedBoolKeys(rowValues), "col_values": sortedBoolKeys(colValues), "matrix": matrix})
	return nil
}

func stringSet(value string) map[string]bool {
	result := map[string]bool{}
	for _, piece := range strings.Split(value, ",") {
		if piece = strings.TrimSpace(piece); piece != "" {
			result[strings.ToUpper(piece)] = true
		}
	}
	return result
}
func pivotValue(task *Task, axis, priority string) string {
	switch axis {
	case "project":
		if task.Project != "" {
			return task.Project
		}
		return "(none)"
	case "prefix":
		if task.Prefix != "" {
			return task.Prefix
		}
		return "(none)"
	case "status":
		return task.Status
	case "priority":
		if priority != "" {
			return priority
		}
		return "(none)"
	}
	return "(none)"
}

func commandRepair(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var fixes multiValue
	fs.Var(&fixes, "fix", "")
	apply := fs.Bool("apply", false, "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if len(fixes) == 0 {
		return fmt.Errorf("repair requires at least one --fix")
	}
	requested := stringSet(strings.Join(fixes, ","))
	allowed := map[string]bool{"NORMALIZE_PRIORITY": true, "RESOLVE_STATUS_MISMATCH": true, "FLAG_SPLIT_BRAIN": true, "REBUILD_BLEVE": true, "REBUILD_WHOOSH": true}
	for fix := range requested {
		if !allowed[fix] {
			return fmt.Errorf("unknown repair fix %q", fix)
		}
	}
	return withLock(store.config.TasksRoot, func() error {
		tasks, err := store.scanTasks()
		if err != nil {
			return err
		}
		actions := []map[string]string{}
		for _, task := range tasks {
			if requested["NORMALIZE_PRIORITY"] && task.Priority != "" && canonicalPriority(task.Priority) != task.Priority {
				actions = append(actions, map[string]string{"task_id": task.ID, "fix": "normalize_priority", "from": task.Priority, "to": canonicalPriority(task.Priority)})
				if *apply {
					task.Priority = canonicalPriority(task.Priority)
					task.Updated = time.Now().Format("2006-01-02")
					if err := store.writeTask(task); err != nil {
						return err
					}
				}
			}
			if requested["RESOLVE_STATUS_MISMATCH"] && stringValue(task.Frontmatter["status"]) != task.Status {
				actions = append(actions, map[string]string{"task_id": task.ID, "fix": "resolve_status_mismatch", "to": task.Status})
				if *apply {
					task.Updated = time.Now().Format("2006-01-02")
					if err := store.writeTask(task); err != nil {
						return err
					}
				}
			}
		}
		var sync map[string]interface{}
		if *apply && (requested["REBUILD_BLEVE"] || requested["REBUILD_WHOOSH"]) {
			sync, err = idx.rebuild(store)
		} else if *apply {
			sync, err = idx.sync(store)
		}
		if err != nil {
			return err
		}
		emit(map[string]interface{}{"dry_run": !*apply, "actions": actions, "sync": sync})
		return nil
	})
}

func commandMigrate(store *Store, idx taskIndex, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	return withLock(store.config.TasksRoot, func() error {
		tasks, err := store.scanTasks()
		if err != nil {
			return err
		}
		actions := []map[string]string{}
		used := map[string]bool{}
		for _, task := range tasks {
			used[strings.ToUpper(task.ID)] = true
		}
		for _, task := range tasks {
			prefix, number := task.Prefix, task.Number
			if prefix == "" && regexp.MustCompile(`^\d+$`).MatchString(task.ID) {
				prefix = store.config.DefaultPrefix
				number, _ = strconv.Atoi(task.ID)
			}
			if prefix == "" || number == 0 {
				continue
			}
			targetID := canonicalTaskID(prefix, number)
			slug := slugify(task.Title)
			if slug == "" {
				slug = task.Slug
			}
			targetPath := filepath.Join(store.config.TasksRoot, task.Status, targetID+"-"+slug+".md")
			if task.ID == targetID && filepath.Clean(task.Path) == filepath.Clean(targetPath) {
				continue
			}
			if _, err := os.Stat(targetPath); err == nil && filepath.Clean(targetPath) != filepath.Clean(task.Path) {
				actions = append(actions, map[string]string{"task_id": task.ID, "path": task.Path, "reason": "collision"})
				continue
			}
			actions = append(actions, map[string]string{"task_id": task.ID, "target_task_id": targetID, "path": task.Path, "target_path": targetPath})
			if *apply {
				oldPath := task.Path
				oldComp := strings.TrimSuffix(oldPath, ".md")
				newComp := strings.TrimSuffix(targetPath, ".md")
				if err := os.Rename(oldPath, targetPath); err != nil {
					return err
				}
				if _, err := os.Stat(oldComp); err == nil {
					if err := os.Rename(oldComp, newComp); err != nil {
						return err
					}
				}
				task.ID = targetID
				task.Prefix = prefix
				task.Number = number
				task.Path = targetPath
				task.CompanionDir = companionIfExists(newComp)
				task.Updated = time.Now().Format("2006-01-02")
				if err := store.writeTask(task); err != nil {
					return err
				}
			}
		}
		var sync map[string]interface{}
		if *apply {
			sync, err = idx.rebuild(store)
			if err != nil {
				return err
			}
		}
		emit(map[string]interface{}{"applied": *apply, "actions": actions, "migrated_count": len(actions), "sync": sync})
		return nil
	})
}

func commandIndex(store *Store, idx taskIndex, args []string) error {
	if len(args) != 1 || (args[0] != "sync" && args[0] != "rebuild") {
		return fmt.Errorf("usage: tasks-cli index sync|rebuild")
	}
	var result map[string]interface{}
	var err error
	if args[0] == "sync" {
		result, err = idx.sync(store)
	} else {
		result, err = idx.rebuild(store)
	}
	if err != nil {
		return err
	}
	emit(result)
	return nil
}

func emit(value interface{}) {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	fmt.Println(string(encoded))
}
func emitError(err error) {
	emit(map[string]interface{}{"error": map[string]string{"code": "task_command_failed", "message": err.Error()}})
}

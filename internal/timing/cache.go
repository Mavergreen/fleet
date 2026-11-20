package timing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pruneAfter is how old a cached run may get before Put deletes it.
const pruneAfter = 120 * 24 * time.Hour

// Cache stores the jobs of completed runs, one JSON file per run at
// <Dir>/<repo>/<runID>.json. Warn, if set, receives one-line warnings.
type Cache struct {
	Dir  string
	Warn io.Writer
}

// DefaultCache returns a cache under os.UserCacheDir()/mavergreen-fleet.
func DefaultCache() (*Cache, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return &Cache{Dir: filepath.Join(d, "mavergreen-fleet")}, nil
}

type entry struct {
	Repo    string    `json:"repo"`
	RunID   int64     `json:"run_id"`
	Attempt int       `json:"attempt"`
	Created time.Time `json:"created"`
	Jobs    []Job     `json:"jobs"`
}

// safeRepo reports whether repo is a single plain path element.
func safeRepo(repo string) bool {
	return repo != "" && repo != "." && repo != ".." && repo == filepath.Base(repo) && !strings.ContainsAny(repo, `/\`)
}

func (c *Cache) path(repo string, runID int64) string {
	return filepath.Join(c.Dir, repo, strconv.FormatInt(runID, 10)+".json")
}

// Get returns the cached jobs for a run at exactly this attempt. A missing,
// corrupt or stale-attempt file is a miss.
func (c *Cache) Get(repo string, runID int64, attempt int) ([]Job, bool) {
	if c == nil || c.Dir == "" || !safeRepo(repo) {
		return nil, false
	}
	b, err := os.ReadFile(c.path(repo, runID))
	if err != nil {
		return nil, false
	}
	var e entry
	if json.Unmarshal(b, &e) != nil || e.RunID != runID || e.Attempt != attempt {
		return nil, false
	}
	return e.Jobs, true
}

// Put stores the jobs of a completed run (the caller decides completeness)
// atomically, then prunes the repo's entries whose run is over 120 days old.
func (c *Cache) Put(run Run, jobs []Job) error {
	if c == nil || c.Dir == "" {
		return errors.New("no cache directory")
	}
	if !safeRepo(run.Repo) {
		return fmt.Errorf("unsafe repo name %q", run.Repo)
	}
	dir := filepath.Join(c.Dir, run.Repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(entry{Repo: run.Repo, RunID: run.ID, Attempt: run.Attempt, Created: run.Created, Jobs: jobs})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	// The mtime carries the run's age so Prune never reads file contents.
	if !run.Created.IsZero() {
		os.Chtimes(tmp.Name(), run.Created, run.Created)
	}
	if err := os.Rename(tmp.Name(), c.path(run.Repo, run.ID)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Prune removes the repo's entries whose run (the file's mtime, set by Put to
// the run's Created) is over 120 days old, and abandoned .tmp-* files over
// an hour old. It never reads file contents.
func (c *Cache) Prune(repo string, now time.Time) error {
	if c == nil || c.Dir == "" || !safeRepo(repo) {
		return fmt.Errorf("unsafe repo name %q", repo)
	}
	dir := filepath.Join(c.Dir, repo)
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, f := range files {
		name := f.Name()
		var limit time.Duration
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			limit = time.Hour
		case strings.HasSuffix(name, ".json"):
			limit = pruneAfter
		default:
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > limit {
			os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}

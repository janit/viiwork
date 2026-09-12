// Package alias is viiwork 2's model aliases (spec section 7): a stable name
// such as stable-coder that points at a real model mesh-wide. The store keeps
// the C6 table durably in state_dir, the merge applies C6's rule to gossiped
// entries, the resolver answers which model an alias means now, and the
// service gossips writes and serves /v1/aliases.
package alias

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

var (
	ErrNotFound  = errors.New("alias: not found")
	ErrNoHistory = errors.New("alias: no previous version to revert to")
	ErrTooMany   = errors.New("alias: table is full")
)

const fileName = "aliases.json"

// fileWriter replaces a file durably; the seam exists to test fsync failures.
type fileWriter func(dir, name string, data []byte) error

// writeFileDurable replaces dir/name so that a crash leaves either the old
// file or the new one: it writes and syncs a temporary file, renames it over
// the target, then syncs the directory so the rename itself survives.
func writeFileDurable(dir, name string, data []byte) (err error) {
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Store is this node's alias table. Every write is on disk before it returns.
type Store struct {
	dir   string
	self  string
	now   func() time.Time
	write fileWriter

	mu    sync.Mutex
	table meshapi.AliasTable
}

// Open loads dir/aliases.json, or starts an empty table when there is none.
// A file it cannot read or does not recognise stops startup rather than being
// replaced (Decision 13).
func Open(dir, self string, now func() time.Time) (*Store, error) {
	return openWith(dir, self, now, writeFileDurable)
}

func openWith(dir, self string, now func() time.Time, write fileWriter) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{dir: dir, self: self, now: now, write: write,
		table: meshapi.AliasTable{V: meshapi.AliasTableVersion, Aliases: map[string]meshapi.AliasEntry{}}}

	path := filepath.Join(dir, fileName)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("alias table %s: %w", path, err)
	default:
		var t meshapi.AliasTable
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("alias table %s: %w", path, err)
		}
		if t.V != meshapi.AliasTableVersion {
			return nil, fmt.Errorf("alias table %s: version %d, this build reads version %d", path, t.V, meshapi.AliasTableVersion)
		}
		if t.Aliases == nil {
			t.Aliases = map[string]meshapi.AliasEntry{}
		}
		s.table = t
	}

	probe := filepath.Join(dir, fileName+".probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return nil, fmt.Errorf("alias state directory %s is not writable: %w", dir, err)
	}
	os.Remove(probe)

	if _, err := s.PurgeTombstones(); err != nil {
		return nil, err
	}
	return s, nil
}

// Table is a deep copy of the whole table.
func (s *Store) Table() meshapi.AliasTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyTable(s.table)
}

// Get returns a deep copy of name's entry, tombstones included.
func (s *Store) Get(name string) (meshapi.AliasEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.table.Aliases[name]
	return copyEntry(e), ok
}

// peek is Get without the copy, for per-request reads. The store never changes
// a stored entry's slices in place (every write stores new ones), so reading
// them is safe; callers must not modify them.
func (s *Store) peek(name string) (meshapi.AliasEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.table.Aliases[name]
	return e, ok
}

// Live counts entries that are not tombstones.
func (s *Store) Live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLocked()
}

func (s *Store) liveLocked() int {
	n := 0
	for _, e := range s.table.Aliases {
		if !e.Deleted {
			n++
		}
	}
	return n
}

// Set writes a new version of name pointing at target.
func (s *Store) Set(name, target string, fallbacks []string) (meshapi.AliasEntry, error) {
	if !meshapi.ValidAliasName(name) {
		return meshapi.AliasEntry{}, fmt.Errorf("alias: invalid name %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.table.Aliases[name]
	if (!had || prev.Deleted) && s.liveLocked() >= meshapi.MaxAliases {
		return meshapi.AliasEntry{}, ErrTooMany
	}
	e := s.nextVersion(prev)
	e.Target = target
	e.Fallbacks = copyStrings(fallbacks)
	e.History = historyAfter(prev, had, prev.History)
	return s.commitLocked(name, e)
}

// Delete writes a tombstone for a live alias.
func (s *Store) Delete(name string) (meshapi.AliasEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.table.Aliases[name]
	if !had || prev.Deleted {
		return meshapi.AliasEntry{}, ErrNotFound
	}
	e := s.nextVersion(prev)
	e.Fallbacks = []string{}
	e.Deleted = true
	e.History = historyAfter(prev, had, prev.History)
	return s.commitLocked(name, e)
}

// Revert makes history[0] current, and the entry it replaces history[0]
// (Decision 5): a second revert undoes the first, and reverting a tombstone
// undeletes the alias.
func (s *Store) Revert(name string) (meshapi.AliasEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.table.Aliases[name]
	if !had {
		return meshapi.AliasEntry{}, ErrNotFound
	}
	if len(prev.History) == 0 {
		return meshapi.AliasEntry{}, ErrNoHistory
	}
	back := prev.History[0]
	e := s.nextVersion(prev)
	e.Target = back.Target
	e.Fallbacks = copyStrings(back.Fallbacks)
	e.History = historyAfter(prev, had, prev.History[1:])
	return s.commitLocked(name, e)
}

// PurgeTombstones removes tombstones older than AliasTombstoneTTL
// (Decision 14) and returns how many it removed.
func (s *Store) PurgeTombstones() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-meshapi.AliasTombstoneTTL).UnixMilli()
	removed := map[string]meshapi.AliasEntry{}
	for name, e := range s.table.Aliases {
		if e.Deleted && e.TS < cutoff {
			removed[name] = e
			delete(s.table.Aliases, name)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := s.persistLocked(); err != nil {
		for name, e := range removed {
			s.table.Aliases[name] = e
		}
		return 0, err
	}
	return len(removed), nil
}

// nextVersion is a new entry by this node, one version past prev.
func (s *Store) nextVersion(prev meshapi.AliasEntry) meshapi.AliasEntry {
	return meshapi.AliasEntry{Ver: prev.Ver + 1, TS: s.now().UnixMilli(), By: s.self, Fallbacks: []string{}}
}

// commitLocked stores e under name and persists the table, restoring the
// previous entry if persisting fails. It returns a copy of e.
func (s *Store) commitLocked(name string, e meshapi.AliasEntry) (meshapi.AliasEntry, error) {
	prev, had := s.table.Aliases[name]
	s.table.Aliases[name] = e
	if err := s.persistLocked(); err != nil {
		s.restoreLocked(name, prev, had)
		return meshapi.AliasEntry{}, err
	}
	return copyEntry(e), nil
}

func (s *Store) restoreLocked(name string, prev meshapi.AliasEntry, had bool) {
	if had {
		s.table.Aliases[name] = prev
	} else {
		delete(s.table.Aliases, name)
	}
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.table, "", "  ")
	if err != nil {
		return err
	}
	if err := s.write(s.dir, fileName, append(data, '\n')); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Join(s.dir, fileName), err)
	}
	return nil
}

// historyAfter is the history of a version that replaces prev: prev itself
// when it was live, then rest, capped at MaxAliasHistory.
func historyAfter(prev meshapi.AliasEntry, had bool, rest []meshapi.AliasVersion) []meshapi.AliasVersion {
	var h []meshapi.AliasVersion
	if had && !prev.Deleted {
		h = append(h, versionOf(prev))
	}
	for _, v := range rest {
		h = append(h, copyVersion(v))
	}
	return capHistory(h)
}

func versionOf(e meshapi.AliasEntry) meshapi.AliasVersion {
	return meshapi.AliasVersion{Target: e.Target, Fallbacks: copyStrings(e.Fallbacks), TS: e.TS, By: e.By}
}

func capHistory(h []meshapi.AliasVersion) []meshapi.AliasVersion {
	if len(h) > meshapi.MaxAliasHistory {
		h = h[:meshapi.MaxAliasHistory]
	}
	return h
}

func copyStrings(in []string) []string {
	return append([]string{}, in...)
}

func copyVersion(v meshapi.AliasVersion) meshapi.AliasVersion {
	v.Fallbacks = copyStrings(v.Fallbacks)
	return v
}

func copyEntry(e meshapi.AliasEntry) meshapi.AliasEntry {
	if e.Fallbacks != nil {
		e.Fallbacks = copyStrings(e.Fallbacks)
	}
	if e.History != nil {
		h := make([]meshapi.AliasVersion, len(e.History))
		for i, v := range e.History {
			h[i] = copyVersion(v)
		}
		e.History = h
	}
	return e
}

func copyTable(t meshapi.AliasTable) meshapi.AliasTable {
	out := meshapi.AliasTable{V: t.V, Aliases: make(map[string]meshapi.AliasEntry, len(t.Aliases))}
	for name, e := range t.Aliases {
		out.Aliases[name] = copyEntry(e)
	}
	return out
}

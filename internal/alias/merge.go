package alias

import (
	"fmt"
	"sort"

	"github.com/janit/viiwork/v2/meshapi"
)

// Conflict is two writes of one alias that never saw each other (the same
// version from different writes). The merge rule picks the winner; the
// service logs the conflict.
type Conflict struct {
	Name   string
	Winner meshapi.AliasEntry
	Loser  meshapi.AliasEntry
}

// undo restores one entry after a failed persist.
type undo struct {
	name string
	prev meshapi.AliasEntry
	had  bool
}

// Merge applies a remote entry to the table by the C6 rule
// (meshapi.CompareAliasEntries) and persists a change before returning. An
// invalid name is ignored, and so is a live entry with no target.
//
// The target check matters because this is the path entries arrive on from
// other members, and it validated only the name. ValidateWrite refuses an
// empty target locally ("target is required"), so without this a member could
// place in every node's table an entry that no node would accept from its own
// operator. A tombstone legitimately carries no target; a live entry must not.
func (s *Store) Merge(name string, remote meshapi.AliasEntry) (changed bool, conflict *Conflict, err error) {
	if !meshapi.ValidAliasName(name) {
		return false, nil, nil
	}
	if remote.Target == "" && !remote.Deleted {
		return false, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed, conflict, u := s.mergeLocked(name, remote)
	if !changed {
		return false, conflict, nil
	}
	if err := s.persistLocked(); err != nil {
		s.restoreLocked(u.name, u.prev, u.had)
		return false, nil, err
	}
	return true, conflict, nil
}

// MergeTable merges a whole remote table under one persist. A persist failure
// rolls every change back (Decision 12). Merges never apply the alias limit
// (Decision 15): every node has to converge on the same table.
func (s *Store) MergeTable(t meshapi.AliasTable) (changed []string, conflicts []Conflict, err error) {
	if t.V != meshapi.AliasTableVersion {
		return nil, nil, fmt.Errorf("alias table version %d, this build merges version %d", t.V, meshapi.AliasTableVersion)
	}
	names := make([]string, 0, len(t.Aliases))
	for name := range t.Aliases {
		if meshapi.ValidAliasName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	s.mu.Lock()
	defer s.mu.Unlock()
	var undos []undo
	for _, name := range names {
		c, conflict, u := s.mergeLocked(name, t.Aliases[name])
		if conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
		if c {
			changed = append(changed, name)
			undos = append(undos, u)
		}
	}
	if len(changed) == 0 {
		return nil, conflicts, nil
	}
	if err := s.persistLocked(); err != nil {
		for i := len(undos) - 1; i >= 0; i-- {
			s.restoreLocked(undos[i].name, undos[i].prev, undos[i].had)
		}
		return nil, nil, err
	}
	return changed, conflicts, nil
}

// mergeLocked changes the in-memory table only, and says how to undo it.
func (s *Store) mergeLocked(name string, remote meshapi.AliasEntry) (bool, *Conflict, undo) {
	local, had := s.table.Aliases[name]
	u := undo{name: name, prev: local, had: had}
	remote = copyEntry(remote)
	if remote.Fallbacks == nil {
		remote.Fallbacks = []string{}
	}
	remote.History = capHistory(remote.History)

	if !had {
		s.table.Aliases[name] = remote
		return true, nil, u
	}

	conflicted := meshapi.IsAliasConflict(remote, local)
	switch cmp := meshapi.CompareAliasEntries(remote, local); {
	case cmp > 0:
		result := remote
		if len(result.History) == 0 {
			// A broadcast carries no history: keep what this node had (Decision 6).
			result.History = historyAfter(local, true, local.History)
		}
		var conflict *Conflict
		if conflicted {
			result.History = withLoserFirst(result.History, local)
			conflict = &Conflict{Name: name, Winner: copyEntry(result), Loser: copyEntry(local)}
		}
		s.table.Aliases[name] = result
		return true, conflict, u

	case cmp < 0:
		if !conflicted {
			return false, nil, u
		}
		result := copyEntry(local)
		result.History = withLoserFirst(result.History, remote)
		conflict := &Conflict{Name: name, Winner: copyEntry(result), Loser: remote}
		if historiesEqual(result.History, local.History) {
			return false, conflict, u
		}
		s.table.Aliases[name] = result
		return true, conflict, u

	default:
		// The same version: unite the histories, so a node that took the
		// history-less broadcast first still gets the history push/pull
		// carries (Decision 6, review finding 2).
		united := unionHistory(local.History, remote.History)
		if historiesEqual(united, local.History) {
			return false, nil, u
		}
		result := copyEntry(local)
		result.History = united
		s.table.Aliases[name] = result
		return true, nil, u
	}
}

type versionKey struct {
	ts int64
	by string
}

// withLoserFirst puts a tie's losing write at the front of the winner's
// history, unless a version with its TS and By is already there. A losing
// tombstone is not kept: a version with no target could only be reverted into
// an alias pointing nowhere.
func withLoserFirst(h []meshapi.AliasVersion, loser meshapi.AliasEntry) []meshapi.AliasVersion {
	if loser.Deleted {
		return h
	}
	for _, v := range h {
		if v.TS == loser.TS && v.By == loser.By {
			return h
		}
	}
	return capHistory(append([]meshapi.AliasVersion{versionOf(loser)}, h...))
}

// unionHistory is both histories without repeats (a version is its TS and
// By), newest first by TS and then larger By, capped.
func unionHistory(a, b []meshapi.AliasVersion) []meshapi.AliasVersion {
	seen := map[versionKey]bool{}
	var out []meshapi.AliasVersion
	for _, list := range [][]meshapi.AliasVersion{a, b} {
		for _, v := range list {
			k := versionKey{v.TS, v.By}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, copyVersion(v))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS > out[j].TS
		}
		return out[i].By > out[j].By
	})
	return capHistory(out)
}

func historiesEqual(a, b []meshapi.AliasVersion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TS != b[i].TS || a[i].By != b[i].By || a[i].Target != b[i].Target || !stringsEqual(a[i].Fallbacks, b[i].Fallbacks) {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

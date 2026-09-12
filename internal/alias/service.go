package alias

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// eventType is the activity event type of alias changes and conflicts.
	eventType = "alias"
	// purgeInterval is how often tombstones past their TTL are removed.
	purgeInterval = time.Hour
	// maxConflictKeys bounds the conflicts remembered to emit each only once.
	maxConflictKeys = 256
)

// Events records activity; *activity.Log satisfies it.
type Events interface {
	Emit(typ string, gpuID int, format string, args ...any)
}

type conflictKey struct {
	name              string
	winnerTS, loserTS int64
	winnerBy, loserBy string
}

// Service is the alias write path and the mesh payload: it validates and
// stores local writes, gossips every change, merges what other nodes send,
// and records changes and conflicts as activity events. It is safe for
// concurrent use; memberlist calls the payload methods from its own goroutines.
//
// P6 opens the store and builds the service before mesh.Start and passes the
// service as Options.Payload, so the node's first push/pull merges its on-disk
// table with the mesh (spec "Durability").
type Service struct {
	store      *Store
	resolver   *Resolver
	broadcast  func(key string, msg []byte)
	events     Events
	logf       func(string, ...any)
	purgeEvery time.Duration

	mu            sync.Mutex
	badMsgLogged  bool
	tooLarge      map[string]bool
	conflicts     map[conflictKey]bool
	conflictOrder []conflictKey
}

var _ mesh.Payload = (*Service)(nil)

func NewService(store *Store, resolver *Resolver, broadcast func(key string, msg []byte), events Events, logf func(string, ...any)) *Service {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Service{
		store: store, resolver: resolver, broadcast: broadcast, events: events, logf: logf,
		purgeEvery: purgeInterval, tooLarge: map[string]bool{}, conflicts: map[conflictKey]bool{},
	}
}

func (s *Service) Store() *Store       { return s.store }
func (s *Service) Resolver() *Resolver { return s.resolver }

// Set validates and writes an alias, then gossips it.
func (s *Service) Set(name string, req meshapi.AliasWriteRequest) (meshapi.AliasBroadcast, error) {
	if err := s.resolver.ValidateWrite(name, req); err != nil {
		return meshapi.AliasBroadcast{}, err
	}
	old, had := s.store.Get(name)
	e, err := s.store.Set(name, req.Target, req.Fallbacks)
	if err != nil {
		return meshapi.AliasBroadcast{}, err
	}
	return s.publish(name, old, had, e), nil
}

// Delete writes a tombstone and gossips it.
func (s *Service) Delete(name string) (meshapi.AliasBroadcast, error) {
	old, had := s.store.Get(name)
	e, err := s.store.Delete(name)
	if errors.Is(err, ErrNotFound) {
		return meshapi.AliasBroadcast{}, &WriteError{Status: 404, Message: fmt.Sprintf("alias %q not found", name)}
	}
	if err != nil {
		return meshapi.AliasBroadcast{}, err
	}
	return s.publish(name, old, had, e), nil
}

// Revert swaps the alias back to its previous version and gossips it.
func (s *Service) Revert(name string) (meshapi.AliasBroadcast, error) {
	old, had := s.store.Get(name)
	e, err := s.store.Revert(name)
	switch {
	case errors.Is(err, ErrNotFound):
		return meshapi.AliasBroadcast{}, &WriteError{Status: 404, Message: fmt.Sprintf("alias %q not found", name)}
	case errors.Is(err, ErrNoHistory):
		return meshapi.AliasBroadcast{}, &WriteError{Status: 409, Message: fmt.Sprintf("alias %q has no previous version to revert to", name)}
	case err != nil:
		return meshapi.AliasBroadcast{}, err
	}
	return s.publish(name, old, had, e), nil
}

// Run purges old tombstones every purge interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.store.PurgeTombstones()
			switch {
			case err != nil:
				s.logf("alias: purging tombstones: %v", err)
			case n > 0:
				s.logf("alias: purged %d tombstones older than %s", n, meshapi.AliasTombstoneTTL)
			}
		}
	}
}

// NotifyMsg merges one broadcast. A change is recorded and gossiped again,
// because memberlist does not re-gossip user messages (Decision 2); a remote
// write is not validated again, since the node that made it did.
func (s *Service) NotifyMsg(msg []byte) {
	var b meshapi.AliasBroadcast
	if err := json.Unmarshal(msg, &b); err != nil {
		s.mu.Lock()
		first := !s.badMsgLogged
		s.badMsgLogged = true
		s.mu.Unlock()
		if first {
			s.logf("alias: ignoring a broadcast that is not valid JSON: %v", err)
		}
		return
	}
	old, had := s.store.Get(b.Name)
	changed, conflict, err := s.store.Merge(b.Name, b.Entry)
	if err != nil {
		s.logf("alias: merging %s: %v", b.Name, err)
		return
	}
	if conflict != nil {
		s.emitConflict(*conflict)
	}
	if changed {
		s.republish(b.Name, old, had)
	}
}

// LocalState is the whole table for push/pull, history included.
func (s *Service) LocalState(join bool) []byte {
	raw, err := json.Marshal(s.store.Table())
	if err != nil {
		s.logf("alias: encoding the table: %v", err)
		return nil
	}
	return raw
}

// MergeRemoteState merges a member's whole table.
func (s *Service) MergeRemoteState(buf []byte, join bool) {
	var t meshapi.AliasTable
	if err := json.Unmarshal(buf, &t); err != nil {
		s.logf("alias: ignoring a pushed table that is not valid JSON: %v", err)
		return
	}
	before := s.store.Table()
	changed, conflicts, err := s.store.MergeTable(t)
	if err != nil {
		s.logf("alias: merging a pushed table: %v", err)
		return
	}
	for _, c := range conflicts {
		s.emitConflict(c)
	}
	for _, name := range changed {
		old, had := before.Aliases[name]
		s.republish(name, old, had)
	}
}

// republish records and gossips a merged change, but only when the current
// version changed: a history-only change (a tie's loser, or histories united)
// is not news, and a broadcast would not carry it anyway.
func (s *Service) republish(name string, old meshapi.AliasEntry, had bool) {
	cur, ok := s.store.Get(name)
	if !ok || (had && cur.Ver == old.Ver && cur.TS == old.TS && cur.By == old.By) {
		return
	}
	s.publish(name, old, had, cur)
}

// publish gossips an entry without its history (Decision 1) and records the
// change.
func (s *Service) publish(name string, old meshapi.AliasEntry, had bool, e meshapi.AliasEntry) meshapi.AliasBroadcast {
	e.History = nil
	b := meshapi.AliasBroadcast{Name: name, Entry: e}
	raw, err := json.Marshal(b)
	switch {
	case err != nil:
		s.logf("alias: encoding %s: %v", name, err)
	case len(raw) <= maxBroadcastBytes:
		if s.broadcast != nil {
			s.broadcast(name, raw)
		}
	default:
		s.mu.Lock()
		first := !s.tooLarge[name]
		s.tooLarge[name] = true
		s.mu.Unlock()
		if first {
			s.logf("alias %s is too large to broadcast (%d bytes); push/pull will carry it", name, len(raw))
		}
	}

	from := "(none)"
	if had && !old.Deleted {
		from = old.Target
	}
	to := e.Target
	if e.Deleted {
		to = "(deleted)"
	}
	if s.events != nil {
		s.events.Emit(eventType, -1, "alias %s: %s -> %s by %s", name, from, to, e.By)
	}
	return b
}

// emitConflict records a conflict once per pair of writes.
func (s *Service) emitConflict(c Conflict) {
	k := conflictKey{name: c.Name, winnerTS: c.Winner.TS, winnerBy: c.Winner.By, loserTS: c.Loser.TS, loserBy: c.Loser.By}
	s.mu.Lock()
	if s.conflicts[k] {
		s.mu.Unlock()
		return
	}
	s.conflicts[k] = true
	s.conflictOrder = append(s.conflictOrder, k)
	if len(s.conflictOrder) > maxConflictKeys {
		delete(s.conflicts, s.conflictOrder[0])
		s.conflictOrder = s.conflictOrder[1:]
	}
	s.mu.Unlock()
	if s.events != nil {
		s.events.Emit(eventType, -1, "alias conflict %s: %s by %s at %s wins over %s by %s at %s", c.Name,
			c.Winner.Target, c.Winner.By, formatUpdatedAt(c.Winner.TS),
			c.Loser.Target, c.Loser.By, formatUpdatedAt(c.Loser.TS))
	}
}

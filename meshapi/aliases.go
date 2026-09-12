package meshapi

import (
	"regexp"
	"time"
)

// Alias table limits (spec contract C6).
const (
	AliasTableVersion = 1
	MaxAliases        = 256
	MaxAliasHistory   = 10
	AliasTombstoneTTL = 30 * 24 * time.Hour
)

// AliasInfo.State values.
const (
	AliasStateOK          = "ok"
	AliasStateFallback    = "fallback"
	AliasStateUnavailable = "unavailable"
	AliasStateShadowed    = "shadowed"
)

// AliasTable is the whole alias table: the push/pull payload and the content
// of state_dir/aliases.json.
type AliasTable struct {
	V       int                   `json:"v"`
	Aliases map[string]AliasEntry `json:"aliases"`
}

// AliasEntry is the current version of one alias. Ver is a per-alias counter:
// a write sets it to the highest Ver the writing node has seen, plus 1. TS is
// unix milliseconds on the writing node and By its node name. A delete is a
// tombstone: Deleted true and Target cleared.
type AliasEntry struct {
	Target    string         `json:"target"`
	Fallbacks []string       `json:"fallbacks"`
	Ver       uint64         `json:"ver"`
	TS        int64          `json:"ts"`
	By        string         `json:"by"`
	Deleted   bool           `json:"deleted"`
	History   []AliasVersion `json:"history,omitempty"`
}

// AliasVersion is a previous version kept in AliasEntry.History, newest first.
type AliasVersion struct {
	Target    string   `json:"target"`
	Fallbacks []string `json:"fallbacks"`
	TS        int64    `json:"ts"`
	By        string   `json:"by"`
}

// AliasBroadcast is one gossip broadcast: a single changed entry.
type AliasBroadcast struct {
	Name  string     `json:"name"`
	Entry AliasEntry `json:"entry"`
}

// AliasesResponse is the body of GET /v1/aliases.
type AliasesResponse struct {
	Aliases []AliasInfo `json:"aliases"`
}

// AliasInfo is one alias as a node currently resolves it. Resolved is null
// when neither the target nor any fallback is served.
type AliasInfo struct {
	Name      string   `json:"name"`
	Target    string   `json:"target"`
	Fallbacks []string `json:"fallbacks"`
	UpdatedAt string   `json:"updated_at"`
	UpdatedBy string   `json:"updated_by"`
	Resolved  *string  `json:"resolved"`
	State     string   `json:"state"`
}

// AliasWriteRequest is the body of PUT /v1/aliases/<name>.
type AliasWriteRequest struct {
	Target    string   `json:"target"`
	Fallbacks []string `json:"fallbacks"`
	Force     bool     `json:"force"`
}

var aliasNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidAliasName reports whether name is an acceptable alias name.
func ValidAliasName(name string) bool { return aliasNameRE.MatchString(name) }

// CompareAliasEntries orders two versions of the same alias by the C6 merge
// rule: higher Ver wins; on equal Ver, higher TS; on equal TS, the larger By.
// It returns +1 when a wins, -1 when b wins, and 0 on a tie in all three.
func CompareAliasEntries(a, b AliasEntry) int {
	switch {
	case a.Ver != b.Ver:
		return winner(a.Ver > b.Ver)
	case a.TS != b.TS:
		return winner(a.TS > b.TS)
	case a.By != b.By:
		return winner(a.By > b.By)
	}
	return 0
}

func winner(aWins bool) int {
	if aWins {
		return 1
	}
	return -1
}

// IsAliasConflict reports two writes that never saw each other: the same Ver
// from different writes, which only a split network or two writes inside the
// gossip delay produce. The merge rule still picks one; a conflict is logged.
func IsAliasConflict(a, b AliasEntry) bool {
	return a.Ver == b.Ver && (a.TS != b.TS || a.By != b.By)
}

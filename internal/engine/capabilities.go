package engine

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This file is the whole set of optional engine capabilities, kept together so
// that it can be read at once. Each is found by type assertion at its call
// site: an engine implements what it can answer honestly and nothing else,
// because a method returning a plausible-looking zero is worse than an absent
// one — the mesh renders absent as "unknown" and zero as fact.
//
// Adding a capability is additive and breaks no existing engine. Widening
// Engine instead would force every engine to implement a method it must then
// lie in.

// OptionsValidator validates this engine's configuration block. path is the
// model's position ("models[0]"), so every error can name the field that is
// wrong — C1's rule does not stop being true because the rule moved into the
// engine. Called during config validation, before anything starts.
type OptionsValidator interface {
	ValidateOptions(path string, s Spec) error
}

// CPURunner is implemented by an engine that can serve with no GPUs. Not
// implementing it means gpus: is required for that engine. The rule was never
// about llama.cpp by name; it is about an engine that can run without a card.
type CPURunner interface {
	RunsOnCPU() bool
}

// TokenProgressReader is implemented by an engine that can report PER-REQUEST
// token progress, and is called in place of Load on every load tick.
// Cumulative token counters are NOT this: a running total in a field the
// dashboard renders as "tokens left in this request" is worse than a blank.
type TokenProgressReader interface {
	LoadProgress(ctx context.Context, s Spec, addr string) (load Load, decoded, remain int64, err error)
}

// GPUBindingReader is implemented by an engine that reports which card it
// actually bound, and is called once on the transition to healthy. A mismatch
// against the host inventory is REPORTED, never acted on: the backend is
// serving correctly, and taking it out of the mesh would trade a wrong label
// for a lost GPU. ok is false when the engine says nothing, which is not a
// mismatch.
type GPUBindingReader interface {
	BoundGPU(ctx context.Context, addr string) (uuid string, ok bool, err error)
}

// Versioner is an engine that can say which version of itself is installed
// and which version this viiwork needs. Optional, like every capability here:
// an engine without it declares no requirement and is never checked
// (FreeToken's rolling nightly has no version to read).
type Versioner interface {
	// MinVersion is the oldest engine this viiwork works with, in the
	// engine's own spelling; "" means no requirement. Raise it in the change
	// that starts relying on a newer engine.
	MinVersion() string
	// Version runs the binary s's options name and reports its version.
	Version(ctx context.Context, s Spec) (string, error)
	// AtLeast reports whether installed satisfies min in this engine's order.
	AtLeast(installed, min string) (bool, error)
}

// UsageReporting says what an engine's streamed chat responses tell the proxy
// about token usage (performance routing, spec §4).
type UsageReporting struct {
	// Unasked: a streamed response ends with a usage chunk even when the
	// client did not set stream_options.include_usage. When false, and
	// CachedTokens is true, the proxy asks for usage and strips the chunk
	// again for a client that did not; without CachedTokens it never asks,
	// because a sample needs the cached count.
	Unasked bool
	// CachedTokens: usage.prompt_tokens_details.cached_tokens is present
	// whenever any prompt token came from cache, so its absence means zero.
	// When false an absent field means "unknown" and the sample is dropped.
	CachedTokens bool
}

// UsageReporter declares how the engine reports token usage, which is what
// performance routing measures with. Optional, like every capability here: an
// engine that does not implement it is assumed to report nothing it was not
// asked for and no cached count, so viiwork never asks it for usage, it
// yields no time-to-first-token sample in practice, and the scored router
// treats it as learning (priced at the fleet median, plus a 5% trickle).
type UsageReporter interface {
	UsageReporting() UsageReporting
}

// UsageOf is e's UsageReporting, or the zero value.
func UsageOf(e Engine) UsageReporting {
	if u, ok := e.(UsageReporter); ok {
		return u.UsageReporting()
	}
	return UsageReporting{}
}

// ReasoningSeparator is implemented by an engine whose streamed responses keep
// reasoning apart from the answer: reasoning arrives as
// delta.reasoning_content with no <think> tags, and the answer follows as
// delta.content. Such a stream needs no rewriting, and for a client that did
// not ask for thinking the proxy passes it through as it is — the client
// shows the reasoning field or ignores it — instead of renaming reasoning to
// content, the rule for every other engine, written for a llama-server that
// may put its whole answer in reasoning_content. Declare it only for a server
// that always writes the answer to content.
type ReasoningSeparator interface {
	SeparatesReasoning() bool
}

// SeparatesReasoning reports whether e declares a separate reasoning channel.
func SeparatesReasoning(e Engine) bool {
	r, ok := e.(ReasoningSeparator)
	return ok && r.SeparatesReasoning()
}

// PerfKeyer is implemented by an engine whose model speed depends on
// something the node's model entry does not show — a config file of the
// engine's own that the entry only names. PerfKey returns a short string that
// changes when that something does; the node adds it to the key its saved
// performance baseline is filed under, so the change drops the baseline as a
// change of args does. "" adds nothing. Called at start and on reload, with
// the Spec ValidateOptions gets.
type PerfKeyer interface {
	PerfKey(s Spec) string
}

// ImageReader is implemented by an engine that can say whether a backend
// reads images (image_url parts of a chat request). It is asked once per
// launch, on the transition to healthy, with the Spec the backend was started
// with. ok is false when the engine cannot say for this backend, which is not
// "no": the router sends a request with an image only to a backend that says
// yes while one exists, to one that cannot say when none does, and never to
// one that says no. An engine that would have to guess does not implement
// this.
type ImageReader interface {
	ReadsImages(ctx context.Context, s Spec, addr string) (reads, ok bool, err error)
}

// ImagesOf asks e whether the backend at addr reads images. ok is false for
// an engine without the capability, for one that cannot say, and on an error.
func ImagesOf(ctx context.Context, e Engine, s Spec, addr string) (reads, ok bool) {
	r, is := e.(ImageReader)
	if !is {
		return false, false
	}
	reads, ok, err := r.ReadsImages(ctx, s, addr)
	if err != nil || !ok {
		return false, false
	}
	return reads, true
}

// WarmUpper is implemented by an engine whose server answers its probe as
// ready before it is safe to send it work. WarmUp is how long a backend must
// have been ready, without a break, before the node calls it healthy; until
// then it stays in starting with phase "warming up", takes no request and
// keeps its place in the load gate, and the wait counts against
// startup_timeout. Zero is no warm-up. Called with the Spec the backend was
// launched with. It applies to every start of a backend, respawns included,
// and never to a backend that is already healthy.
type WarmUpper interface {
	WarmUp(s Spec) time.Duration
}

// WarmUpOf is e's warm-up for s, or zero.
func WarmUpOf(e Engine, s Spec) time.Duration {
	if w, ok := e.(WarmUpper); ok {
		if d := w.WarmUp(s); d > 0 {
			return d
		}
	}
	return 0
}

// DisplayNamer is implemented by an engine to say what it is called by
// people: "llama.cpp" where Name is "llamacpp". Name stays the id that
// configuration and the mesh use; this is the spelling a dashboard shows,
// and only the engine's own package knows it. The conformance kit requires
// it, because every engine can answer it.
type DisplayNamer interface {
	DisplayName() string
}

// DisplayName is the display name of the engine registered under id, or id
// itself when there is no such engine or it declares none: a reader gets
// the id rather than nothing.
func DisplayName(id string) string {
	if e, ok := Lookup(id); ok {
		if d, ok := e.(DisplayNamer); ok {
			if n := strings.TrimSpace(d.DisplayName()); n != "" {
				return n
			}
		}
	}
	return id
}

// SessionSharer is implemented by an engine that can write a client
// session's conversation to a file and park such a file in its own cache,
// so that another node can take a session over warm (kv_share). The node
// moves the files between members; the engine only saves and imports them,
// and decides whether a later prompt matches what it parked.
type SessionSharer interface {
	// SessionShare is the model's kv_share settings; ok is false when the
	// model shares nothing. Called at start and on reload with the Spec
	// ValidateOptions gets, after Command has accepted the same file.
	SessionShare(s Spec) (cfg SessionShareConfig, ok bool)
	// SaveSession asks the backend at addr to save the conversation that
	// `session` (the client's raw X-Session-Affinity or X-Session-Id) last
	// continued, as the file `name` in SaveDir. With a base (a save id the
	// receiver holds) the engine may write a delta against it instead of a
	// whole file, and says which it wrote. ErrSessionGone when the backend
	// no longer holds the conversation, which is not a failure.
	SaveSession(ctx context.Context, addr, session, name, base string) (SavedSession, error)
	// ImportSession parks the file `name` in SaveDir in the backend's cache
	// without touching what it is serving.
	ImportSession(ctx context.Context, addr, name string) error
}

// SessionHolder is implemented by a SessionSharer that can say what a
// backend still holds of a session's conversation. An engine's cache evicts
// on its own, so an import made earlier proves nothing about the next turn;
// without this the node assumes an imported replica stays held.
type SessionHolder interface {
	// SessionHeld is how many tokens the backend at addr still holds of the
	// longest conversation whose first n token ids (little-endian int32)
	// hash to `hash` (FNV-1a 64), 0 for none: the replica's resume point (its
	// deepest checkpoint's ids), since
	// the branch `session` last continued there can be another one (a
	// compaction elsewhere). With n = 0 the engine names the conversation by
	// `session` alone. An error means the backend cannot say (an engine
	// without the query, or a timeout).
	SessionHeld(ctx context.Context, addr, session string, n int64, hash uint64) (int64, error)
}

// SavedSession is what a save wrote.
type SavedSession struct {
	// ID names the save; the next save may name it as its base. "" when
	// the engine gives saves no ids.
	ID string
	// Delta: the file is a delta against the base asked for (STRSDLT), not
	// a whole session file.
	Delta bool
}

// SessionShareConfig is one model's kv_share settings, as its engine reads
// them.
type SessionShareConfig struct {
	// Group names the members that replicate to each other. Members agree
	// on it, and on the model and engine configuration behind it.
	Group string
	// Transport is the kvshare transport spec ("http").
	Transport string
	// StoreDir holds the replicas this node receives; "" is
	// <state_dir>/kvshare/<model>.
	StoreDir string
	// MaxBytes bounds StoreDir; the least recently received go first.
	MaxBytes int64
	// MinTokens: a shorter conversation is neither sent nor imported.
	MinTokens int64
	// SaveDir is the directory the backend saves and imports session files
	// in, as an absolute path the node can read and write.
	SaveDir string
	// MaxFileBytes is the largest session file the model can write; a
	// member's put declaring more is refused.
	MaxFileBytes int64
}

// ErrSessionGone: the backend does not hold the session's conversation
// (never saw it, or another conversation has taken its place).
var ErrSessionGone = errors.New("engine: the session's conversation is no longer held")

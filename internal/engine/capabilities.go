package engine

import "context"

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

// Package meshapi is the wire contract of the viiwork mesh.
//
// Everything a node publishes to other nodes and to the mesh dashboard is
// defined here: the /v1/status payload, the /v1/cluster snapshot, the activity
// event stream, the prompt lookup, and the endpoint paths themselves. viiwork
// itself consumes this package through type aliases in internal/peer and
// internal/activity, so there is exactly one definition of every field name
// that travels between hosts.
//
// # Why this is a public package
//
// The mesh is not a viiwork-only protocol. A node is anything that answers
// /v1/status with this shape, and the fleet already has a second
// implementation: viiwork-nvidia, which drives vLLM on CUDA hardware and joins
// the same mesh and the same dashboard. internal/ cannot be imported across
// module boundaries, so the contract lives out here where both can depend on
// it. viiwork remains the source of truth — a field is added here first, and
// other implementations follow.
//
// # Compatibility rules
//
// The mesh is a fleet of independently upgraded machines, so at any moment
// several versions of this contract are live on the wire at once. Two rules
// keep that survivable, and both are load bearing:
//
//   - **New fields are additive and omitempty.** A node running an older build
//     omits what it does not know about, and consumers degrade to blanks for
//     that host rather than breaking. Every field added since v1.0 follows
//     this, which is why the dashboard can render a mixed-version fleet.
//
//   - **Absent is not zero.** A consumer must read a missing numeric field as
//     "unknown", never as a measured zero. PromptHistory is the canonical case:
//     zero means "this node is too old to say", not "this node keeps no
//     prompts", and treating it as the latter silently evicts rows the owning
//     node could still answer a lookup for.
//
// Renaming or repurposing an existing field is a breaking change with no
// migration path, because the two ends of the wire are upgraded hours or days
// apart. Add a new field and leave the old one populated instead.
//
// # What is not here
//
// Types that never leave a process stay in their own packages. The balancer's
// BackendState, the process manager's Backend and the config structs are all
// internal to an implementation: two nodes agree on what they *say* to each
// other, not on how either arrives at it. viiwork picks routes by llama.cpp
// slot occupancy and viiwork-nvidia by vLLM's scheduler queue; the mesh only
// ever sees the resulting InFlight count.
//
// The energy store is the one other shared package, and it is shared for the
// same reason rather than as part of this contract. Its records are local
// persistence, not wire: a store directory never crosses a host boundary, and
// what does cross is only the EnergyKWh24h and EnergyKWh30d totals defined
// here. github.com/janit/viiwork/energy is public because a second
// implementation would otherwise have to reimplement the on-disk format, which
// is the same drift problem this package solves for the wire. PowerSource is
// where the two meet: it names which reading a node settled on, and the store
// records the same label beside its own history.
package meshapi

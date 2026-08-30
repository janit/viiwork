// Package energy is viiwork's durable energy accounting: how many kWh a host
// drew, and which models drew them.
//
// A node samples whole-node power and per-GPU power on an interval, averages
// each minute, splits the measured node figure between the models causing it
// and the baseline that would be drawn anyway, and writes the result to a
// fixed-size on-disk history covering the last 24 hours by the minute and the
// last year by the hour and the day. The store never grows: 2.6 MB for a
// 10-GPU host, preallocated at creation.
//
// Wiring one up is three calls:
//
//	store, err := energy.Open(energy.Config{
//		Dir:    "/var/lib/viiwork/energy",
//		GPUIDs: []int{0, 1, 2, 3},
//		Source: "dcmi",
//	}, nil)
//	if err != nil {
//		return err
//	}
//	defer store.Close()
//
//	rec := energy.NewRecorder(store, 30*time.Second, nodeWatts, gpuReadings, nil)
//	go rec.Run(ctx) // flushes the bucket in progress when ctx is cancelled
//
//	kwh := store.KWh24h() // whole-node energy, rolling 24 hours
//	per := store.ByModel(energy.TierDay, monthAgo, now)
//
// # Why this is a public package
//
// It is here for the same reason [meshapi] and the web dashboards are: the
// fleet has a second implementation. viiwork-nvidia drives vLLM on CUDA
// hardware, joins the same mesh and publishes the same energy_kwh_24h, and Go
// forbids importing another module's internal/ tree. The alternative was
// reimplementing the ring, tier and model-table machinery there, which means
// two independent producers of one binary format with nothing keeping them in
// step. viiwork remains the source of truth for the format; other
// implementations follow it.
//
// The on-disk layout is therefore contract, not an implementation detail.
// docs/energy-store-format.md in the viiwork repository specifies it
// byte-by-byte, including the rules for changing it. In short: the record
// sizes, the header layout and the "VIIWENG1" magic are frozen, and any change
// to them bumps the magic so that a mismatched build refuses to open the store
// instead of silently reinitialising a year of somebody else's history.
//
// # The seam a second implementation fills
//
// This package never runs a command or reads a sensor. It takes two functions
// and knows nothing else about where power comes from:
//
//   - [NodeWattsFunc] reports current whole-node draw and whether it is
//     measurable at all. Returning false is how a node with no usable reading
//     records nothing, rather than accumulating a confident zero.
//   - [GPUReadingsFunc] reports each card's current draw and which model is
//     resident on it.
//
// viiwork fills them from ipmitool and rocm-smi; viiwork-nvidia fills them from
// nvidia-smi. Nothing else differs, which is the argument for one shared
// package rather than two.
//
// # What NodeRecord.Watts means, and why the store says so
//
// The two implementations put physically different quantities in that field.
// viiwork writes whole-chassis draw over IPMI or DCMI — CPU, fans, drives and
// PSU losses included, around 400 W on an idle 10-GPU host. An implementation
// with no BMC writes the sum of GPU board power instead, which excludes all of
// that and understates the wall figure substantially. The bytes are identical
// either way.
//
// [Config.Source] labels which it was, using the same vocabulary as the mesh
// wire field of the same name, and [Store.Source] reads it back. Provenance is
// also visible at the mesh layer through meshapi's PowerSource, but a store
// directory copied off a host for inspection carries no mesh context, so the
// label is recorded beside the data. An empty label means unknown, never a
// default — see [Store.Source].
//
// # Attribution
//
// [Attribute] and [Store.Floors] exist for the whole-chassis case: one measured
// figure has to be divided between the models responsible and a baseline that
// would be drawn anyway. Each GPU is charged in proportion to how far it sits
// above its idle floor, the residual is reported as [Attribution.BaselineW]
// rather than smeared across models, and the split always reconciles — baseline
// plus every share equals the measured node power, so no total is invented.
//
// An implementation that measures each board directly does not need either
// call: a card's draw is its model's draw, with nothing to infer. Such a
// producer supplies [GPURecord.AttrW] and RawW as the same measured value and
// leaves the baseline to the node series.
//
// Absolute accuracy is around ±15%, dominated by how the node reading itself is
// obtained. Compare models within a host freely; compare across hosts with
// care; do not present it as billing grade.
//
// # Operational constraints
//
// Node wattage is a whole-host measurement. A host running several instances of
// a node — one per model, as viiwork deploys — must enable this on exactly one
// of them, or the same chassis draw is recorded several times over. For the
// same reason a recorder should cover every GPU the host reports rather than
// only the cards its own process owns: the marginal-power denominator has to
// span the host, or a co-tenant's load is charged to this instance's models.
//
// A [Store] is safe for concurrent use. One [Recorder] owns one store.
//
// [meshapi]: https://pkg.go.dev/github.com/janit/viiwork/meshapi
package energy

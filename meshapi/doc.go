// Package meshapi is the wire contract of the viiwork 2 mesh: everything a
// node publishes to other nodes, to the gateway and to the dashboards (spec
// contracts C4, C5 and C6).
//
// Rules, all load bearing:
//
//   - Field names are frozen from v2.0.0-alpha.1. wire_test.go enforces it.
//     Renaming or removing a field has no migration path, because the two
//     ends of the wire upgrade at different times. Add a field instead.
//   - Fields added after alpha.1, and fields a node may be unable to
//     measure, are omitempty. Absent means "this node cannot say", never a
//     measured zero.
//   - The package is stdlib-only and imports nothing from internal/, because
//     viiwork-gateway imports it from another module.
//
// Event, MeshEvent, PromptEntry and the activity message helpers are carried
// over from v1 unchanged.
package meshapi

// Package proxy is viiwork 2's engine-agnostic inference proxy (spec section
// 5): the OpenAI-compatible handler that parses a request, verifies a forward,
// runs the dispatch-and-retry loop over the router, executes locally or
// forwards to a peer, rewrites thinking on the executing node, captures output
// and counts requests and tokens.
//
// The hot-path helpers in thinking.go, capture.go and body.go were copied from
// v1's proxy package and keep its measured allocation behaviour; the
// benchmarks in bench_test.go guard them.
package proxy

package logging

import "os"

// debugEnabled gates the [debug] log lines that sit on per-request and
// per-token code paths.
//
// These were unconditional. Every routing decision wrote a line to stdout, and
// under Docker stdout goes through the container log driver — a pipe write plus
// json-file encoding, serialized on the logger's mutex, once per request. On a
// 4-core host shared with llama-server that is CPU taken directly from
// inference, and it also serializes concurrent requests against each other at
// exactly the moment the fleet is trying to run five backends in parallel.
//
// The lines are genuinely useful when diagnosing routing, so they are gated
// rather than deleted: set VIIWORK_DEBUG=1 to restore the previous behaviour.
// Read once at startup — this is checked on hot paths and must not touch the
// environment per call.
var debugEnabled = os.Getenv("VIIWORK_DEBUG") != ""

// DebugEnabled reports whether verbose per-request [debug] logging is on.
func DebugEnabled() bool { return debugEnabled }

package accept

// Acceptance validates a config file for the node that will run it, so its
// tests must register the same engines a node does. The binary does this in
// cmd/viiwork-accept; this is its test-side counterpart.
import _ "github.com/janit/viiwork/v2/internal/engine/llamacpp"

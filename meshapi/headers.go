package meshapi

import "strconv"

// HTTP headers and query parameters of contract C5.
const (
	// HeaderForwarded marks a mesh forward (request, node to node). It is
	// honoured only with a valid HeaderAuth signature in a secured mesh, or
	// from an alive member's source IP in an open mesh.
	HeaderForwarded = "X-Viiwork-Forwarded"
	// HeaderAuth carries the v1.8 meshauth signature.
	HeaderAuth = "X-Viiwork-Auth"
	// HeaderNode is the node that executed the request (response). v1.8
	// meshauth also sends a request header of this name, the signer's node
	// id; the two directions never mix.
	HeaderNode = "X-Viiwork-Node"
	// HeaderOrigin is the node that received the request from the client,
	// present when it was forwarded (response).
	HeaderOrigin = "X-Viiwork-Origin"
	// HeaderGPUBackend is the executing backend's id, see BackendID (response).
	HeaderGPUBackend = "X-Gpu-Backend"
	// HeaderQueuedMs is the time spent in the origin queue (response).
	HeaderQueuedMs = "X-Viiwork-Queued-Ms"
	// HeaderAlias is the alias the client asked for, when one was resolved.
	HeaderAlias = "X-Viiwork-Alias"
	// HeaderModel is the real model that answered.
	HeaderModel = "X-Viiwork-Model"
	// QueryHost pins a request to a node by name (?host=gb1).
	QueryHost = "host"
)

// BackendID names backend index of model, e.g. "Qwen3.8-27B/0".
func BackendID(model string, index int) string {
	return model + "/" + strconv.Itoa(index)
}

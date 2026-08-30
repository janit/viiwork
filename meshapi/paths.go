package meshapi

// Endpoint paths every mesh node serves. They are constants here rather than
// string literals at each call site because both ends of the wire use them:
// a node routes on them and, as a peer, dials them on other nodes. A path
// typo'd in one place and not the other is the kind of mismatch that shows up
// as a silently unreachable host rather than as a build failure.
const (
	// PathStatus is the node-state poll. This is the one endpoint a peer must
	// answer to exist in the mesh at all: Registry.PollOnce dials it on every
	// configured peer, and a node that does not respond here is marked
	// unreachable no matter what else it serves.
	PathStatus = "/v1/status"

	// PathCluster is the whole-mesh snapshot as one node sees it: its own
	// state plus every peer's last poll. Served by every node, which is what
	// makes any single reachable node enough to view the fleet.
	PathCluster = "/v1/cluster"

	// PathModels is the OpenAI-compatible model list, aggregated across the
	// mesh rather than local-only, so a client pointed at any node sees every
	// model the fleet serves.
	PathModels = "/v1/models"

	// Inference entry points. A node accepts these for any model in the mesh,
	// serving locally or forwarding to the peer that owns the model.
	PathChatCompletions = "/v1/chat/completions"
	PathCompletions     = "/v1/completions"
	PathEmbeddings      = "/v1/embeddings"

	// PathHealth is the liveness probe. Deliberately separate from
	// PathStatus: health is cheap and answers "is this process up", while
	// status assembles backend, GPU and power state.
	PathHealth = "/health"

	// PathActivity is the recent event history as a plain JSON read, and
	// PathActivityStream is the live SSE feed of the same events. A node
	// merging the mesh subscribes to peers' streams; see MeshEvent.
	PathActivity       = "/v1/activity"
	PathActivityStream = "/v1/activity/stream"

	// PathMeshStream is the dashboard's single connection: named SSE events
	// carrying merged activity from every node and periodic cluster
	// snapshots. Fan-out happens here, on the server, because the browser may
	// not be able to reach peers directly and EventSource is CORS-bound.
	PathMeshStream = "/v1/mesh/stream"

	// PathPrompts looks up one captured prompt and output by request id on the
	// node that minted it. PathMeshPrompt is the fan-out form, taking an addr
	// and forwarding — but only to an address already in the node's peer list,
	// because it fetches what it is handed on a LAN that also carries IPMI.
	PathPrompts    = "/v1/prompts"
	PathMeshPrompt = "/v1/mesh/prompt"

	// Chassis power control. PathPower is the executor, acting on the node's
	// own host in-band. PathMeshPower is the entry point, forwarding to the
	// node on the target host and falling back to the BMC over the network —
	// which is the only case that can reach a host that is powered off.
	PathPower     = "/v1/power"
	PathMeshPower = "/v1/mesh/power"
)

// Named SSE event types on PathMeshStream. The stream carries two kinds of
// message on one connection, so a consumer must dispatch on the event name;
// an unnamed listener sees neither.
const (
	// SSEActivity carries a MeshEvent.
	SSEActivity = "activity"
	// SSECluster carries a ClusterResponse. Snapshots are diffed server-side
	// and sent only when something changed, so any field that ticks
	// continuously has to earn its place in the payload.
	SSECluster = "cluster"
)

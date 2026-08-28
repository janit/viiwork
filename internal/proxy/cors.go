package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// CORS lets browser code on another origin read this node's API.
//
// viiwork has no authentication: reachability is the authorization model, and
// the fleet is expected to sit on a tailnet. That is what makes an origin
// allowlist meaningful here rather than theatre — it is not protecting the API
// from a caller who can already reach it with curl, it is stopping a random
// page in a tailnet member's browser from quietly driving the fleet through
// that member's network position. Keep the list to origins that are themselves
// only reachable, or only served, where you would accept that.
//
// Two endpoint families need this and are easy to forget:
//
//   - The SSE streams (/v1/mesh/stream and friends). EventSource is CORS-bound
//     like any other fetch, but sends no preflight, so it needs the header on
//     the GET response itself.
//   - OPTIONS. Before this existed the router matched only GET and POST, so
//     every preflight 404'd and no cross-origin POST could work at all.
type CORS struct {
	// Origins are host patterns, not URLs. A leading "*." matches any
	// subdomain ("*.example.com" matches app.example.com but not example.com
	// itself); anything else must match the host exactly.
	Origins []string
	// TailnetIPs additionally allows origins addressed by a literal Tailscale
	// IP. MagicDNS names are the common case, but a tailnet is just as often
	// browsed by its 100.x address, and an allowlist that quietly failed for
	// half the ways you reach the same host would be worse than no allowlist.
	TailnetIPs bool
}

// Tailscale hands out IPv4 from the CGNAT block 100.64.0.0/10 and IPv6 from
// fd7a:115c:a1e0::/48.
var (
	tailnetV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	tailnetV6 = mustCIDR("fd7a:115c:a1e0::/48")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("proxy: bad built-in CIDR " + s + ": " + err.Error())
	}
	return n
}

// exposedHeaders are the viiwork-specific response headers a browser client is
// allowed to read. Without this a cross-origin fetch can see the body but not
// which backend served it, which is most of what those headers are for.
const exposedHeaders = "X-GPU-Backend, X-Queue-Depth, X-Viiwork-Origin, X-Pipeline, X-Pipeline-Locale, X-Pipeline-Steps"

// Allows reports whether origin (an Origin header value) may read this API.
func (c *CORS) Allows(origin string) bool {
	if c == nil || origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Only the two schemes a browser sends for a page fetch. Anything else is
	// an extension origin or a non-browser caller spelling Origin by hand,
	// neither of which should widen the surface.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if c.TailnetIPs {
		if ip := net.ParseIP(host); ip != nil {
			if tailnetV4.Contains(ip) || tailnetV6.Contains(ip) {
				return true
			}
		}
	}
	for _, pat := range c.Origins {
		if strings.HasPrefix(pat, "*.") {
			// Subdomains only. Letting a "*." rule also match the bare apex
			// would silently widen "*.ts.net" into "ts.net", which is somebody
			// else's domain.
			suffix := pat[1:]
			if len(host) > len(suffix) && strings.HasSuffix(strings.ToLower(host), suffix) {
				return true
			}
			continue
		}
		if strings.EqualFold(host, pat) {
			return true
		}
	}
	return false
}

// apply writes the CORS response headers for a request and reports whether it
// was a preflight that has now been fully answered.
//
// Vary: Origin is set whether or not the origin is allowed. The response
// genuinely differs by origin, and a shared cache that missed that would hand
// one origin's allow header to another.
func (c *CORS) apply(w http.ResponseWriter, r *http.Request) (handled bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	w.Header().Add("Vary", "Origin")
	allowed := c.Allows(origin)
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", exposedHeaders)
	}

	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	// From here on this is a preflight, which never reaches a route handler.
	if !allowed {
		// A bare 204 with no allow header would fail in the browser too, but
		// identically to a dozen other mistakes. A 403 says which one it was,
		// which matters when the fix is a one-line config change on a host you
		// are not currently looking at.
		http.Error(w, `{"error":{"message":"origin not allowed","type":"forbidden"}}`, http.StatusForbidden)
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		w.Header().Set("Access-Control-Allow-Headers", req)
	} else {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

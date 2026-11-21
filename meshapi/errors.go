package meshapi

// ErrorBody.Type values used by the router.
const (
	ErrTypeRateLimit   = "rate_limit"
	ErrTypeUnavailable = "unavailable"
)

// ErrorResponse is the OpenAI-style error body, e.g.
// {"error":{"type":"rate_limit","message":"no free slot"}}.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	// Code (v2.13) is a machine-readable reason, as in OpenAI's errors,
	// when the type alone does not say it (ErrCodeModelModality). Absent on
	// every other error.
	Code string `json:"code,omitempty"`
}

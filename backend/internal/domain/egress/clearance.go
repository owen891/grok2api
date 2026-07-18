package egress

import "time"

// ClearanceBundle is the transport-neutral clearance material produced for a
// single egress lease. Token and cookies are deliberately kept separate so a
// future browser/cookie provider can be added without changing registration.
type ClearanceBundle struct {
	Mode      string     `json:"mode"`
	Token     string     `json:"token,omitempty"`
	Cookies   string     `json:"cookies,omitempty"`
	UserAgent string     `json:"user_agent,omitempty"`
	NodeID    uint64     `json:"node_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

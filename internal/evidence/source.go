// Package evidence describes source completeness without coupling collectors to
// a particular API, domain, or output format.
package evidence

// Error contains a safe classification and message. Collectors must not place
// raw subprocess stderr or credential-bearing API diagnostics in these fields.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Source records the scope and success of an individual evidence collection.
// Complete describes the declared source scope; it is not a correctness verdict
// or a claim that independently fetched sources form an atomic snapshot.
type Source struct {
	Complete    bool     `json:"complete"`
	CapturedAt  string   `json:"captured_at"`
	Requests    int      `json:"requests"`
	Items       int      `json:"items"`
	Error       *Error   `json:"error,omitempty"`
	Limitations []string `json:"limitations,omitempty"`
}

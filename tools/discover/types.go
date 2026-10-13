// Package discover prepares redacted evidence for agent-led workflow discovery.
package discover

import "time"

// PrepareOptions controls deterministic evidence preparation. Salt must be a
// persistent, installation-local secret of at least 16 bytes.
type PrepareOptions struct {
	Provider       string
	Project        string
	Days           int
	AllProjects    bool
	Salt           []byte
	Now            time.Time
	MaxRecords     int
	MaxRecordBytes int
}

// Bundle is redacted evidence, not a semantic assessment of recurring work.
type Bundle struct {
	SchemaVersion int      `json:"schema_version"`
	PreparedAt    string   `json:"prepared_at"`
	Coverage      Coverage `json:"coverage"`
	Records       []Record `json:"records"`
	Groups        []Group  `json:"groups"`
}

type Record struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	ProjectID   string   `json:"project_id"`
	SessionID   string   `json:"session_id"`
	Timestamp   string   `json:"timestamp"`
	Kind        string   `json:"kind"`
	Text        string   `json:"text"`
	ResourceIDs []string `json:"resource_ids"`
	Redacted    bool     `json:"redacted"`
	Truncated   bool     `json:"truncated"`
	ReplayOf    string   `json:"replay_of,omitempty"`
}

// Groups are overlapping lexical leads. Their counts must not be added.
type Group struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Terms     []string `json:"terms"`
	RecordIDs []string `json:"record_ids"`
	Records   int      `json:"records"`
	Sessions  int      `json:"sessions"`
	Projects  int      `json:"projects"`
	Incidents int      `json:"resource_or_session_units"`
}

type Coverage struct {
	RequestedSince        string   `json:"requested_since"`
	RequestedUntil        string   `json:"requested_until"`
	ObservedSince         string   `json:"observed_since"`
	ObservedUntil         string   `json:"observed_until"`
	InputRecords          int      `json:"input_records"`
	RetainedRecords       int      `json:"retained_records"`
	RejectedRecords       int      `json:"rejected_records"`
	DuplicateRecords      int      `json:"duplicate_records"`
	PossibleReplayRecords int      `json:"possible_replay_records"`
	RedactedRecords       int      `json:"redacted_records"`
	TruncatedRecords      int      `json:"truncated_records"`
	Sessions              int      `json:"sessions"`
	Projects              int      `json:"projects"`
	Complete              bool     `json:"complete"`
	Warnings              []string `json:"warnings"`
}

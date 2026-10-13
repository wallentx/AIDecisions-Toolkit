// Package ci collects bounded, read-only evidence for one GitHub Actions run attempt.
package ci

import (
	"context"
	"time"

	"aid/internal/evidence"
	"aid/internal/github"
)

const SchemaVersion = 1

const (
	SourceRun         = "run"
	SourceJobs        = "jobs"
	SourceLogs        = "logs"
	SourceAnnotations = "annotations"
	SourceArtifacts   = "artifacts"
	SourceFreshness   = "freshness"
)

var sourceKeys = []string{
	SourceRun,
	SourceJobs,
	SourceLogs,
	SourceAnnotations,
	SourceArtifacts,
	SourceFreshness,
}

// API is the read-only GitHub transport used by Collect.
type API interface {
	REST(context.Context, string, any) error
	RawREST(context.Context, string) ([]byte, error)
}

// Options bounds API work and retained normalized evidence. Zero values use
// defaults. Positive values are hard limits and are never raised.
type Options struct {
	PerPage           int
	MaxPages          int
	MaxJobs           int
	MaxLogJobs        int
	MaxRequests       int
	MaxRetainedBytes  int
	MaxAnnotations    int
	MaxArtifacts      int
	MaxLogBytes       int
	MaxExcerpts       int
	ContextLines      int
	MaxExcerptLines   int
	MaxLineCharacters int
	MaxLogLines       int
}

// DefaultOptions returns the bounded collector defaults.
func DefaultOptions() Options {
	return Options{
		PerPage:           100,
		MaxPages:          20,
		MaxJobs:           500,
		MaxLogJobs:        20,
		MaxRequests:       200,
		MaxRetainedBytes:  16 << 20,
		MaxAnnotations:    500,
		MaxArtifacts:      500,
		MaxLogBytes:       2 << 20,
		MaxExcerpts:       5,
		ContextLines:      3,
		MaxExcerptLines:   60,
		MaxLineCharacters: 2000,
		MaxLogLines:       100_000,
	}
}

// Snapshot is a versioned CI evidence document. Complete applies only to the
// declared selection scope and is not a root-cause judgment.
type Snapshot struct {
	SchemaVersion int                        `json:"schema_version"`
	Reference     github.WorkflowRun         `json:"reference"`
	Selection     Selection                  `json:"selection"`
	Provenance    Provenance                 `json:"provenance"`
	StartedAt     time.Time                  `json:"started_at"`
	FinishedAt    time.Time                  `json:"finished_at"`
	Complete      bool                       `json:"complete"`
	Sources       map[string]evidence.Source `json:"sources"`
	Run           WorkflowRunEvidence        `json:"run"`
	Freshness     Freshness                  `json:"freshness"`
	Jobs          []Job                      `json:"jobs"`
	Annotations   []Annotation               `json:"annotations"`
	Artifacts     ArtifactCollection         `json:"artifacts"`
	Output        OutputState                `json:"output"`
}

type Selection struct {
	Scope       string `json:"scope"`
	JobID       int64  `json:"job_id,omitempty"`
	Description string `json:"description"`
}

type Provenance struct {
	AttemptSelection string `json:"attempt_selection"`
	PinnedAttempt    int    `json:"pinned_attempt"`
	ReadOnly         bool   `json:"read_only"`
	Atomic           bool   `json:"atomic"`
	Limitation       string `json:"limitation"`
}

type WorkflowRunEvidence struct {
	ID           int64      `json:"id"`
	Attempt      int        `json:"attempt"`
	WorkflowID   int64      `json:"workflow_id"`
	Name         string     `json:"name"`
	DisplayTitle string     `json:"display_title,omitempty"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion,omitempty"`
	HeadSHA      string     `json:"head_sha"`
	HeadBranch   string     `json:"head_branch,omitempty"`
	URL          string     `json:"url,omitempty"`
	HTMLURL      string     `json:"html_url,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"run_started_at,omitempty"`
}

type Freshness struct {
	Start         RunIdentity `json:"start"`
	End           RunIdentity `json:"end"`
	ChangedFields []string    `json:"changed_fields,omitempty"`
	Ongoing       bool        `json:"ongoing"`
}

type RunIdentity struct {
	ID           int64      `json:"id"`
	Attempt      int        `json:"attempt"`
	WorkflowID   int64      `json:"workflow_id"`
	HeadSHA      string     `json:"head_sha"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"run_started_at,omitempty"`
}

type Job struct {
	ID              int64       `json:"id"`
	RunID           int64       `json:"run_id"`
	Attempt         int         `json:"run_attempt"`
	Name            string      `json:"name"`
	HeadSHA         string      `json:"head_sha"`
	Status          string      `json:"status"`
	Conclusion      string      `json:"conclusion,omitempty"`
	StartedAt       *time.Time  `json:"started_at,omitempty"`
	CompletedAt     *time.Time  `json:"completed_at,omitempty"`
	HTMLURL         string      `json:"html_url,omitempty"`
	CheckRunURL     string      `json:"check_run_url,omitempty"`
	RunnerName      string      `json:"runner_name,omitempty"`
	Labels          []string    `json:"labels"`
	Steps           []Step      `json:"steps"`
	FailedStepNames []string    `json:"failed_step_names"`
	Log             LogEvidence `json:"log"`
}

type Step struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type LogEvidence struct {
	State                    string          `json:"state"`
	Reason                   string          `json:"reason"`
	RetrievalComplete        bool            `json:"retrieval_complete"`
	ExcerptSelectionComplete bool            `json:"excerpt_selection_complete"`
	BytesReceived            int             `json:"bytes_received"`
	LinesObserved            int             `json:"lines_observed"`
	OmittedLines             int             `json:"omitted_lines,omitempty"`
	TruncatedLines           int             `json:"truncated_lines,omitempty"`
	Excerpts                 []LogExcerpt    `json:"excerpts"`
	OmittedExcerpts          int             `json:"omitted_excerpts,omitempty"`
	Error                    *evidence.Error `json:"error,omitempty"`
}

type LogExcerpt struct {
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Lines           []string `json:"lines"`
	SelectionReason string   `json:"selection_reason"`
}

type Annotation struct {
	CaptureOrdinal        int    `json:"capture_ordinal"`
	SourcePage            int    `json:"source_page"`
	SourceIndex           int    `json:"source_index"`
	JobID                 int64  `json:"job_id"`
	CheckRunID            int64  `json:"check_run_id"`
	Path                  string `json:"path,omitempty"`
	StartLine             int    `json:"start_line,omitempty"`
	EndLine               int    `json:"end_line,omitempty"`
	StartColumn           int    `json:"start_column,omitempty"`
	EndColumn             int    `json:"end_column,omitempty"`
	AnnotationLevel       string `json:"annotation_level"`
	Message               string `json:"message"`
	Title                 string `json:"title,omitempty"`
	RawDetails            string `json:"raw_details,omitempty"`
	BlobURL               string `json:"blob_url,omitempty"`
	Scope                 string `json:"scope"`
	HistoricalAttribution string `json:"historical_attribution"`
}

type ArtifactCollection struct {
	Scope              string     `json:"scope"`
	AttemptAttribution bool       `json:"attempt_attribution"`
	Limitation         string     `json:"limitation"`
	Items              []Artifact `json:"items"`
}

type Artifact struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	SizeBytes int64      `json:"size_in_bytes"`
	Expired   bool       `json:"expired"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	URL       string     `json:"url,omitempty"`
	Digest    string     `json:"digest,omitempty"`
}

type OutputState struct {
	MaxBytes     int            `json:"max_bytes"`
	Truncated    bool           `json:"truncated"`
	OmittedItems map[string]int `json:"omitted_items"`
	RetryHint    string         `json:"retry_hint,omitempty"`
}

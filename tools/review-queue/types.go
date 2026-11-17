// Package reviewqueue lists open pull requests waiting on a reviewer and tags
// each one as new work or a re-review, with a compact delta for re-reviews.
// It reads GitHub through the shared gh transport and never mutates anything.
package reviewqueue

import (
	"time"

	"aid/internal/prdelta"
)

const SchemaVersion = 1

// API is the read-only GitHub transport shared with internal/prdelta.
type API = prdelta.API

// Scope sources, in inference order.
const (
	ScopeFlag              = "flag"
	ScopeConfig            = "config"
	ScopeWorkspaceRepo     = "workspace_repo"
	ScopeWorkspaceChildren = "workspace_children"
	ScopeAll               = "all"
)

// Item kinds.
const (
	KindNew      = "new"
	KindReReview = "re_review"
	KindUnknown  = "unknown"
)

// Source statuses that are not GitHub error kinds.
const (
	StatusOK               = "ok"
	StatusSkipped          = "skipped"
	StatusIncomplete       = "incomplete"
	StatusDependencyFailed = "dependency_failed"
)

// Result is the versioned queue document.
type Result struct {
	SchemaVersion  int         `json:"schema_version"`
	Reviewer       string      `json:"reviewer"`
	ReviewerSource string      `json:"reviewer_source"`
	Scope          Scope       `json:"scope"`
	AsOf           time.Time   `json:"as_of"`
	Summary        Summary     `json:"summary"`
	Items          []Item      `json:"items"`
	Truncated      bool        `json:"truncated"`
	Sources        Sources     `json:"sources"`
	Complete       bool        `json:"complete"`
	Warnings       []string    `json:"warnings"`
	Output         *OutputInfo `json:"output,omitempty"`
}

// Scope records which owners were searched and which inference rule chose them.
type Scope struct {
	Owners    []string `json:"owners"`
	Source    string   `json:"source"`
	Workspace string   `json:"workspace"`
	Query     string   `json:"query"`
}

type Summary struct {
	Total             int `json:"total"`
	New               int `json:"new"`
	ReReview          int `json:"re_review"`
	ReReviewUnchanged int `json:"re_review_unchanged"`
	Unknown           int `json:"unknown"`
}

// Item is one open pull request with a pending review request.
type Item struct {
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	Title        string `json:"title"`
	TitleClipped bool   `json:"title_clipped,omitempty"`
	Author       string `json:"author"`
	URL          string `json:"url"`
	Draft        bool   `json:"draft"`
	RequestedVia string `json:"requested_via"`
	UpdatedAt    string `json:"updated_at"`
	Kind         string `json:"kind"`
	Delta        *Delta `json:"delta"`
}

// Delta is the compact pr-delta comparison for a re-review.
type Delta struct {
	BaselineState                    string   `json:"baseline_state,omitempty"`
	BaselineCommit                   string   `json:"baseline_commit,omitempty"`
	HeadRelation                     string   `json:"head_relation"`
	Verdict                          string   `json:"verdict"`
	ChangedFiles                     int      `json:"changed_files"`
	ThreadsUnresolved                int      `json:"threads_unresolved"`
	ThreadsUnresolvedWithAuthorReply int      `json:"threads_unresolved_with_author_reply"`
	CI                               DeltaCI  `json:"ci"`
	Complete                         bool     `json:"complete"`
	Warnings                         []string `json:"warnings,omitempty"`
	IncompleteSources                []string `json:"incomplete_sources,omitempty"`
}

// DeltaCI mirrors pr-delta's CI counts at the current head. Other holds
// conclusions such as timed_out, action_required, and stale.
type DeltaCI struct {
	Success   int `json:"success"`
	Failure   int `json:"failure"`
	Pending   int `json:"pending"`
	Skipped   int `json:"skipped"`
	Neutral   int `json:"neutral"`
	Cancelled int `json:"cancelled"`
	Other     int `json:"other"`
}

// Sources reports each evidence source. Reviewer appears only when the
// authenticated user had to be looked up.
type Sources struct {
	Reviewer *SourceStatus `json:"reviewer,omitempty"`
	Search   SourceStatus  `json:"search"`
	Delta    DeltaSource   `json:"delta"`
}

type SourceStatus struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Requests int    `json:"requests"`
	Items    int    `json:"items"`
}

type DeltaSource struct {
	Status     string `json:"status"`
	Computed   int    `json:"computed"`
	Incomplete int    `json:"incomplete"`
	Skipped    int    `json:"skipped"`
}

type OutputInfo struct {
	MaxBytes     int    `json:"max_bytes"`
	Truncated    bool   `json:"truncated"`
	OmittedItems int    `json:"omitted_items"`
	RetryHint    string `json:"retry_hint,omitempty"`
}

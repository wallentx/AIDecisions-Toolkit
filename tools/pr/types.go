// Package pr collects bounded, read-only evidence for a GitHub pull request.
package pr

import (
	"context"
	"time"

	"aid/internal/evidence"
	"aid/internal/github"
)

const SchemaVersion = 1

const (
	SourcePullRequest   = "pull_request"
	SourceFiles         = "files"
	SourceChecks        = "checks"
	SourceStatuses      = "statuses"
	SourceReviewThreads = "review_threads"
	SourceIssueComments = "issue_comments"
	SourceReviews       = "reviews"
	SourceFreshness     = "freshness"
	SourceOpenFindings  = "open_findings"
)

// API is the read-only GitHub transport used by Collect.
type API interface {
	REST(context.Context, string, any) error
	GraphQL(context.Context, string, map[string]any, any) error
}

// Options bounds API work and retained normalized evidence. A zero value uses
// the defaults. Positive values are hard limits and are never raised.
type Options struct {
	PerPage          int
	MaxPages         int
	MaxItems         int
	MaxTotalComments int
	MaxRequests      int
	MaxRetainedBytes int
}

// DefaultOptions returns the collector's bounded defaults.
func DefaultOptions() Options {
	return Options{
		PerPage:          100,
		MaxPages:         50,
		MaxItems:         10_000,
		MaxTotalComments: 25_000,
		MaxRequests:      200,
		MaxRetainedBytes: 16 << 20,
	}
}

// Snapshot is normalized evidence plus per-source completeness and freshness.
type Snapshot struct {
	SchemaVersion int                        `json:"schema_version"`
	Reference     github.PullRequest         `json:"reference"`
	StartedAt     time.Time                  `json:"started_at"`
	FinishedAt    time.Time                  `json:"finished_at"`
	Complete      bool                       `json:"complete"`
	Sources       map[string]evidence.Source `json:"sources"`
	PullRequest   Metadata                   `json:"pull_request"`
	Freshness     Freshness                  `json:"freshness"`
	Files         []File                     `json:"files"`
	Checks        []CheckRun                 `json:"checks"`
	Statuses      []CommitStatus             `json:"statuses"`
	ReviewThreads []ReviewThread             `json:"review_threads"`
	IssueComments []Comment                  `json:"issue_comments"`
	Reviews       []Review                   `json:"reviews"`
	OpenFindings  *OpenFindings              `json:"open_findings,omitempty"`
}

type Metadata struct {
	ID             int64     `json:"id"`
	NodeID         string    `json:"node_id"`
	Number         int       `json:"number"`
	URL            string    `json:"url"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	Merged         bool      `json:"merged"`
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	Author         Actor     `json:"author"`
	UpdatedAt      time.Time `json:"updated_at"`
	ChangedFiles   int       `json:"changed_files"`
	Head           Branch    `json:"head"`
	Base           Branch    `json:"base"`
}

type Branch struct {
	Ref        string             `json:"ref"`
	SHA        string             `json:"sha"`
	Repository RepositoryIdentity `json:"repository"`
}

type RepositoryIdentity struct {
	ID            int64  `json:"id"`
	NodeID        string `json:"node_id"`
	NameWithOwner string `json:"name_with_owner"`
	URL           string `json:"url"`
}

type Actor struct {
	Type   string `json:"type,omitempty"`
	Login  string `json:"login"`
	ID     int64  `json:"id,omitempty"`
	NodeID string `json:"node_id,omitempty"`
	URL    string `json:"url,omitempty"`
}

type Freshness struct {
	Start         IdentityVersion `json:"start"`
	End           IdentityVersion `json:"end"`
	ChangedFields []string        `json:"changed_fields,omitempty"`
}

type IdentityVersion struct {
	HeadSHA        string             `json:"head_sha"`
	HeadRef        string             `json:"head_ref"`
	HeadRepository RepositoryIdentity `json:"head_repository"`
	BaseSHA        string             `json:"base_sha"`
	BaseRef        string             `json:"base_ref"`
	BaseRepository RepositoryIdentity `json:"base_repository"`
	UpdatedAt      time.Time          `json:"updated_at"`
	State          string             `json:"state"`
	Draft          bool               `json:"draft"`
	Merged         bool               `json:"merged"`
}

type File struct {
	SHA         string `json:"sha"`
	Filename    string `json:"filename"`
	Status      string `json:"status"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	Changes     int    `json:"changes"`
	BlobURL     string `json:"blob_url,omitempty"`
	RawURL      string `json:"raw_url,omitempty"`
	ContentsURL string `json:"contents_url,omitempty"`
}

type CheckRun struct {
	ID          int64             `json:"id"`
	NodeID      string            `json:"node_id"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Conclusion  string            `json:"conclusion,omitempty"`
	HeadSHA     string            `json:"head_sha"`
	DetailsURL  string            `json:"details_url,omitempty"`
	ExternalID  string            `json:"external_id,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	CheckSuite  CheckSuite        `json:"check_suite"`
	App         App               `json:"app"`
	ObservedIn  []RepositoryScope `json:"observed_in"`
}

type CheckSuite struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	HeadSHA string `json:"head_sha,omitempty"`
}

type App struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Owner   Actor  `json:"owner"`
	HTMLURL string `json:"url,omitempty"`
}

type CommitStatus struct {
	ID          int64             `json:"id"`
	NodeID      string            `json:"node_id"`
	State       string            `json:"state"`
	Context     string            `json:"context"`
	Description string            `json:"description,omitempty"`
	TargetURL   string            `json:"target_url,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Creator     Actor             `json:"creator"`
	ObservedIn  []RepositoryScope `json:"observed_in"`
}

// RepositoryScope reuses the transport's repository identity for provenance.
type RepositoryScope = github.Repository

type ReviewThread struct {
	ID             string          `json:"id"`
	URL            string          `json:"url,omitempty"`
	Path           string          `json:"path,omitempty"`
	Line           *int            `json:"line,omitempty"`
	OriginalLine   *int            `json:"original_line,omitempty"`
	Resolved       bool            `json:"resolved"`
	Outdated       bool            `json:"outdated"`
	Comments       []Comment       `json:"comments"`
	CommentsSource evidence.Source `json:"comments_source"`
}

type Comment struct {
	ReviewNodeID      string     `json:"review_node_id,omitempty"`
	ID                int64      `json:"id,omitempty"`
	FullDatabaseID    string     `json:"full_database_id,omitempty"`
	NodeID            string     `json:"node_id,omitempty"`
	URL               string     `json:"url,omitempty"`
	Author            Actor      `json:"author"`
	AuthorAssociation string     `json:"author_association,omitempty"`
	Body              string     `json:"body"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Path              string     `json:"path,omitempty"`
	Line              *int       `json:"line,omitempty"`
	OriginalLine      *int       `json:"original_line,omitempty"`
	Side              string     `json:"side,omitempty"`
	StartLine         *int       `json:"start_line,omitempty"`
	DiffHunk          string     `json:"diff_hunk,omitempty"`
	CommitID          string     `json:"commit_id,omitempty"`
	OriginalCommitID  string     `json:"original_commit_id,omitempty"`
	Resolved          bool       `json:"resolved,omitempty"`
	Outdated          bool       `json:"outdated,omitempty"`
	SubmittedAt       *time.Time `json:"submitted_at,omitempty"`
}

type Review struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	URL         string     `json:"url,omitempty"`
	Author      Actor      `json:"author"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	CommitID    string     `json:"commit_id,omitempty"`
}

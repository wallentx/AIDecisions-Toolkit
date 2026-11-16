// Package prdelta reports what changed in a GitHub pull request since a
// reviewer's last review. It collects read-only evidence and never judges code.
package prdelta

import (
	"context"
	"time"

	"aid/internal/evidence"
	"aid/internal/github"
)

const SchemaVersion = 1

// Source keys. A source appears in Result.Sources only when it was needed.
const (
	SourcePullRequest  = "pull_request"
	SourceReviewer     = "reviewer"
	SourceBaseline     = "baseline"
	SourceHeadRelation = "head_relation"
	SourcePRDiff       = "pr_diff"
	SourceCommits      = "commit_equivalence"
	SourceThreads      = "threads"
	SourceChecks       = "ci_checks"
	SourceStatuses     = "ci_statuses"
	SourceStack        = "stack"
)

// Head relations derived from the baseline...head compare status.
const (
	RelationIdentical   = "identical"
	RelationFastForward = "fast_forward"
	RelationRebased     = "rebased"
	RelationBehind      = "behind"
	RelationUnknown     = "unknown"
)

// PR diff verdicts and per-file statuses.
const (
	VerdictUnchanged = "unchanged"
	VerdictMovedOnly = "moved_only"
	VerdictChanged   = "changed"
	VerdictUnknown   = "unknown"
	FileAdded        = "added"
	FileRemoved      = "removed"
)

// API is the read-only GitHub transport used by Collect.
type API interface {
	REST(context.Context, string, any) error
	GraphQL(context.Context, string, map[string]any, any) error
}

// Options bounds collection. Zero values use DefaultOptions.
type Options struct {
	Reviewer         string
	SinceCommit      string
	MaxPages         int
	MaxRequests      int
	MaxCommitPatches int
	MaxHunksPerFile  int
	MaxNames         int
	TextLimit        int
}

// DefaultOptions returns the collector's bounded defaults.
func DefaultOptions() Options {
	return Options{MaxPages: 10, MaxRequests: 150, MaxCommitPatches: 40, MaxHunksPerFile: 20, MaxNames: 50, TextLimit: 280}
}

// Result is the versioned evidence document.
type Result struct {
	SchemaVersion  int                        `json:"schema_version"`
	Reference      github.PullRequest         `json:"reference"`
	Reviewer       string                     `json:"reviewer"`
	ReviewerSource string                     `json:"reviewer_source"`
	StartedAt      time.Time                  `json:"started_at"`
	FinishedAt     time.Time                  `json:"finished_at"`
	Complete       bool                       `json:"complete"`
	PR             PRInfo                     `json:"pr"`
	Baseline       *Baseline                  `json:"baseline"`
	HeadRelation   string                     `json:"head_relation"`
	NewCommits     []CommitRef                `json:"new_commits"`
	NewCommitsNote string                     `json:"new_commits_note,omitempty"`
	PRDiff         *PRDiff                    `json:"pr_diff"`
	Threads        Threads                    `json:"threads"`
	CI             CI                         `json:"ci"`
	Stack          Stack                      `json:"stack"`
	HeadStable     HeadStable                 `json:"head_stable"`
	Sources        map[string]evidence.Source `json:"sources"`
	Warnings       []string                   `json:"warnings"`
	Output         *OutputInfo                `json:"output,omitempty"`
}

type PRInfo struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	BaseRef        string `json:"base_ref"`
	BaseSHA        string `json:"base_sha"`
	HeadRef        string `json:"head_ref"`
	HeadSHA        string `json:"head_sha"`
	DefaultBranch  string `json:"default_branch"`
	ReviewDecision string `json:"review_decision"`
	MergeState     string `json:"merge_state"`
}

type Baseline struct {
	Source      string     `json:"source"`
	ReviewID    *int64     `json:"review_id"`
	ReviewState string     `json:"review_state,omitempty"`
	Commit      string     `json:"commit"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type CommitRef struct {
	SHA              string `json:"sha"`
	MessageFirstLine string `json:"message_first_line"`
}

type PRDiff struct {
	Verdict      string         `json:"verdict"`
	Reason       string         `json:"reason,omitempty"`
	Base         DiffBase       `json:"base"`
	BaselineSide DiffSide       `json:"baseline_side"`
	HeadSide     DiffSide       `json:"head_side"`
	Counts       map[string]int `json:"counts"`
	Files        []FileDelta    `json:"files"`
}

// DiffBase names the comparison base. Source is base_ref, default_branch, or
// recorded_base_sha (closed and merged pull requests).
type DiffBase struct {
	Ref    string `json:"ref"`
	Source string `json:"source"`
}

// DiffSide describes one three-dot compare. Base differs from DiffBase.Ref only
// when the baseline side was realigned past rewritten base commits.
type DiffSide struct {
	Commit    string `json:"commit"`
	Base      string `json:"base"`
	MergeBase string `json:"merge_base"`
	Status    string `json:"status"`
	Commits   int    `json:"commits"`
	Files     int    `json:"files"`
}

type FileDelta struct {
	Path         string       `json:"path"`
	Status       string       `json:"status"`
	ChangedHunks []HunkChange `json:"changed_hunks,omitempty"`
	HunksOmitted int          `json:"changed_hunks_omitted,omitempty"`
	Reason       string       `json:"reason,omitempty"`
}

// HunkChange pairs an unmatched baseline-side hunk header (old) with an
// unmatched head-side hunk header (new). Either may be empty.
type HunkChange struct {
	OldHeader string `json:"old_header"`
	NewHeader string `json:"new_header"`
}

type Threads struct {
	Total               int      `json:"total"`
	Unresolved          int      `json:"unresolved"`
	UnresolvedWithReply int      `json:"unresolved_with_reply"`
	Items               []Thread `json:"items"`
}

type Thread struct {
	ThreadID           string `json:"thread_id"`
	FirstCommentID     int64  `json:"first_comment_id"`
	Path               string `json:"path"`
	Line               *int   `json:"line"`
	IsResolved         bool   `json:"is_resolved"`
	IsOutdated         bool   `json:"is_outdated"`
	AuthorRepliedAfter bool   `json:"author_replied_after"`
	LastReplyAuthor    string `json:"last_reply_author,omitempty"`
	LastReplyExcerpt   string `json:"last_reply_excerpt,omitempty"`
	LastReplyClipped   bool   `json:"last_reply_clipped,omitempty"`
}

type CI struct {
	HeadSHA           string     `json:"head_sha"`
	Counts            CICounts   `json:"counts"`
	NonSuccess        []CIRecord `json:"non_success"`
	NonSuccessOmitted int        `json:"non_success_omitted,omitempty"`
}

type CICounts struct {
	Success   int `json:"success"`
	Failure   int `json:"failure"`
	Pending   int `json:"pending"`
	Skipped   int `json:"skipped"`
	Neutral   int `json:"neutral"`
	Cancelled int `json:"cancelled"`
	Other     int `json:"other"`
}

type CIRecord struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Conclusion string `json:"conclusion"`
}

type Stack struct {
	BaseIsDefaultBranch bool    `json:"base_is_default_branch"`
	BaseBranchExists    *bool   `json:"base_branch_exists"`
	BasePR              *BasePR `json:"base_pr"`
}

type BasePR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

type HeadStable struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Stable bool   `json:"stable"`
}

type OutputInfo struct {
	MaxBytes     int            `json:"max_bytes"`
	Truncated    bool           `json:"truncated"`
	OmittedItems map[string]int `json:"omitted_items"`
	RetryHint    string         `json:"retry_hint,omitempty"`
}

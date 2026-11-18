package pr

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"aid/internal/evidence"
)

// OpenFindings is a derived view of collected threads and review bodies. It
// describes evidence, not whether a finding is valid or blocks merging.
type OpenFindings struct {
	HeadSHA                string        `json:"head_sha"`
	UnresolvedThreads      []Finding     `json:"unresolved_threads"`
	UnthreadedReviewBodies []ReviewBody  `json:"unthreaded_review_bodies"`
	Counts                 FindingCounts `json:"counts"`
	CountsScope            string        `json:"counts_scope"`
	SourceNames            []string      `json:"source_names"`
	Complete               bool          `json:"complete"`
}

type FindingCounts struct {
	Bot              int `json:"bot"`
	Human            int `json:"human"`
	Unknown          int `json:"unknown"`
	Outdated         int `json:"outdated"`
	UnthreadedBodies int `json:"unthreaded_bodies"`
}

type Finding struct {
	DiffSide              string   `json:"diff_side,omitempty"`
	ThreadID              string   `json:"thread_id"`
	URL                   string   `json:"url"`
	Author                string   `json:"author"`
	AuthorKind            string   `json:"author_kind"`
	Bot                   *string  `json:"bot"`
	Path                  string   `json:"path"`
	Line                  *int     `json:"line"`
	Commit                string   `json:"commit"`
	Outdated              bool     `json:"outdated"`
	Severity              *string  `json:"severity"`
	ParseStatus           string   `json:"parse_status"`
	Excerpt               string   `json:"excerpt"`
	ExcerptClipped        bool     `json:"excerpt_clipped"`
	Untrusted             bool     `json:"untrusted"`
	LastReplyBy           *string  `json:"last_reply_by"`
	PRAuthorRepliedAfter  bool     `json:"pr_author_replied_after"`
	RepliesComplete       bool     `json:"replies_complete"`
	SameLocationAs        []string `json:"same_location_as"`
	SameLocationTruncated bool     `json:"same_location_truncated,omitempty"`
}

type ReviewBody struct {
	ReviewID       int64   `json:"review_id"`
	ReviewNodeID   string  `json:"review_node_id"`
	URL            string  `json:"url"`
	Author         string  `json:"author"`
	AuthorKind     string  `json:"author_kind"`
	Bot            *string `json:"bot"`
	Commit         string  `json:"commit"`
	State          string  `json:"state"`
	Label          string  `json:"label"`
	ThreadMatch    string  `json:"thread_match"`
	ParseStatus    string  `json:"parse_status"`
	Excerpt        string  `json:"excerpt"`
	ExcerptClipped bool    `json:"excerpt_clipped"`
	Untrusted      bool    `json:"untrusted"`
}

var findingSources = []string{SourcePullRequest, SourceReviewThreads, SourceReviews, SourceFreshness}

func loginKey(login string) string {
	return strings.TrimSuffix(strings.ToLower(login), "[bot]")
}

func authorKind(actor Actor) (string, *string) {
	isBot := strings.EqualFold(actor.Type, "Bot") || strings.HasSuffix(strings.ToLower(actor.Login), "[bot]")
	if !isBot {
		if actor.Login != "" && strings.EqualFold(actor.Type, "User") {
			return "human", nil
		}
		return "unknown", nil
	}
	var bot string
	switch loginKey(actor.Login) {
	case "copilot", "copilot-pull-request-reviewer":
		bot = "copilot"
	case "chatgpt-codex-connector":
		bot = "codex"
	case "coderabbitai":
		bot = "coderabbit"
	case "claude":
		bot = "claude"
	default:
		return "bot", nil
	}
	return "bot", &bot
}

// REST app logins carry [bot], while GraphQL Bot logins can omit it.
// Never merge a known human and bot just because their display logins match.
func sameActor(a, b Actor) bool {
	if a.Login == "" || b.Login == "" {
		return false
	}
	if a.NodeID != "" && b.NodeID != "" {
		return a.NodeID == b.NodeID
	}
	aKind, _ := authorKind(a)
	bKind, _ := authorKind(b)
	if aKind != "unknown" && bKind != "unknown" && aKind != bKind {
		return false
	}
	if aKind == "bot" && bKind == "bot" {
		return loginKey(a.Login) == loginKey(b.Login)
	}
	return strings.EqualFold(a.Login, b.Login)
}

// Only the leading Codex badge carries inline severity. A changed image marker
// is reported as unrecognized instead of guessing from prose elsewhere.
var codexBadge = regexp.MustCompile(`^(?:\*\*)?(?:<sub>)*(?:\[)?!\[P([0-3]) Badge\]\([^\s()]+\)`)

func findingSeverity(bot *string, body string) (*string, string) {
	if bot == nil || *bot != "codex" {
		return nil, "not_applicable"
	}
	body = strings.TrimSpace(body)
	if match := codexBadge.FindStringSubmatch(body); match != nil {
		severity := "P" + match[1]
		return &severity, "ok"
	}
	if strings.HasPrefix(strings.TrimLeft(body, "*["), "![") || strings.Contains(body, "Badge") {
		return nil, "unrecognized"
	}
	return nil, "not_present"
}

func findingExcerpt(body string, limit int) (string, bool) {
	if utf8.RuneCountInString(body) <= limit {
		return body, false
	}
	return string([]rune(body)[:limit]), true
}

// Comments are in timestamp order. Connection order identifies the root, but
// cannot establish whether a reply with the same timestamp was posted later.
func recordFindingReplies(f *Finding, comments []Comment, prAuthor Actor) {
	reviewer := comments[0].Author
	boundary := comments[0].CreatedAt
	for _, c := range comments[1:] {
		if sameActor(c.Author, reviewer) {
			boundary = c.CreatedAt
		}
	}
	if reviewer.Login == "" || prAuthor.Login == "" || boundary.IsZero() {
		f.RepliesComplete = false
	}
	var latest *Comment
	ambiguousLatest := false
	for i := range comments[1:] {
		c := &comments[i+1]
		if sameActor(c.Author, reviewer) {
			continue
		}
		if c.CreatedAt.IsZero() || boundary.IsZero() || c.CreatedAt.Equal(boundary) {
			f.RepliesComplete = false
			continue
		}
		if c.CreatedAt.Before(boundary) {
			continue
		}
		if c.Author.Login == "" {
			f.RepliesComplete = false
		}
		if sameActor(c.Author, prAuthor) {
			f.PRAuthorRepliedAfter = true
		}
		if latest == nil || c.CreatedAt.After(latest.CreatedAt) {
			latest, ambiguousLatest = c, false
		} else if !sameActor(c.Author, latest.Author) {
			ambiguousLatest = true
		}
	}
	if ambiguousLatest {
		f.RepliesComplete = false
	} else if latest != nil && latest.Author.Login != "" {
		login := latest.Author.Login
		f.LastReplyBy = &login
	}
}

// deriveOpenFindings runs before presentation clipping so markers and review
// identities are interpreted from the complete retained source, never excerpts.
func deriveOpenFindings(snapshot Snapshot, textLimit, maxBytes int) (OpenFindings, evidence.Source) {
	result := OpenFindings{HeadSHA: snapshot.PullRequest.Head.SHA, UnresolvedThreads: []Finding{}, UnthreadedReviewBodies: []ReviewBody{}, CountsScope: "collected", SourceNames: append([]string{}, findingSources...), Complete: true}
	source := evidence.Source{Complete: true, CapturedAt: snapshot.FinishedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
	incomplete := func(reason string) {
		result.Complete = false
		source.Complete = false
		for _, existing := range source.Limitations {
			if existing == reason {
				return
			}
		}
		source.Limitations = append(source.Limitations, reason)
	}
	for _, name := range findingSources {
		if !snapshot.Sources[name].Complete {
			incomplete("incomplete_source:" + name)
		}
	}
	if result.HeadSHA == "" {
		incomplete("head_sha_unavailable")
	}
	matchedReviews := map[string]bool{}
	matchesComplete := snapshot.Sources[SourceReviewThreads].Complete
	threads := append([]ReviewThread{}, snapshot.ReviewThreads...)
	sort.Slice(threads, func(i, j int) bool { return threads[i].ID < threads[j].ID })
	for _, thread := range threads {
		if !thread.CommentsSource.Complete || len(thread.Comments) == 0 {
			matchesComplete = false
			incomplete("incomplete_thread_comments")
		}
		for _, comment := range thread.Comments {
			if comment.ReviewNodeID == "" {
				matchesComplete = false
			} else {
				matchedReviews[comment.ReviewNodeID] = true
			}
		}
		if thread.Resolved {
			continue
		}
		finding := Finding{ThreadID: thread.ID, URL: thread.URL, Path: thread.Path, Line: thread.Line, DiffSide: thread.DiffSide, Outdated: thread.Outdated, Untrusted: true, AuthorKind: "unknown", ParseStatus: "not_present", SameLocationAs: []string{}, RepliesComplete: thread.CommentsSource.Complete}
		if len(thread.Comments) > 0 {
			comments := append([]Comment{}, thread.Comments...)
			sortComments(comments)
			first := comments[0]
			finding.Author = first.Author.Login
			finding.AuthorKind, finding.Bot = authorKind(first.Author)
			finding.Commit = first.CommitID
			if finding.URL == "" {
				finding.URL = first.URL
			}
			if thread.Outdated {
				finding.Commit = first.OriginalCommitID
				finding.Line = thread.OriginalLine
			}
			finding.Severity, finding.ParseStatus = findingSeverity(finding.Bot, first.Body)
			if finding.ParseStatus == "unrecognized" {
				incomplete("unrecognized_severity_marker")
			}
			finding.Excerpt, finding.ExcerptClipped = findingExcerpt(first.Body, textLimit)
			recordFindingReplies(&finding, comments, snapshot.PullRequest.Author)
		} else {
			finding.RepliesComplete = false
		}
		if !finding.RepliesComplete {
			incomplete("reply_state_incomplete")
		}
		switch finding.AuthorKind {
		case "bot":
			result.Counts.Bot++
		case "human":
			result.Counts.Human++
		default:
			result.Counts.Unknown++
		}
		if finding.Outdated {
			result.Counts.Outdated++
		}
		result.UnresolvedThreads = append(result.UnresolvedThreads, finding)
	}
	groupLocations(result.UnresolvedThreads, maxBytes, incomplete)
	reviews := append([]Review{}, snapshot.Reviews...)
	sort.Slice(reviews, func(i, j int) bool {
		if reviews[i].ID != reviews[j].ID {
			return reviews[i].ID < reviews[j].ID
		}
		return reviews[i].NodeID < reviews[j].NodeID
	})
	for _, review := range reviews {
		if strings.EqualFold(review.State, "PENDING") || strings.TrimSpace(review.Body) == "" {
			continue
		}
		kind, bot := authorKind(review.Author)
		label, parse, body := "unthreaded_review_body", "not_applicable", review.Body
		// CodeRabbit can attach outside-diff findings to a review that also has
		// inline threads. Preserve that section even when the review ID is matched.
		outside := -1
		malformedOutside := false
		if bot != nil && *bot == "coderabbit" {
			outside = strings.Index(body, "Outside diff range comments")
			malformedOutside = outside < 0 && strings.Contains(strings.ToLower(body), "outside diff range")
		}
		matched := review.NodeID != "" && matchedReviews[review.NodeID]
		if outside >= 0 {
			label, parse = "outside_diff_range", "ok"
			if matched {
				body = body[outside:]
			}
		}
		if malformedOutside {
			label, parse = "unrecognized_review_body", "unrecognized"
			incomplete("unrecognized_review_body_marker")
		}
		if matched && outside < 0 && !malformedOutside {
			continue
		}
		match := "unmatched"
		if matched {
			match = "matched"
		} else if !matchesComplete || review.NodeID == "" {
			match = "unknown"
			incomplete("review_thread_match_incomplete")
		}
		excerpt, clipped := findingExcerpt(body, textLimit)
		result.UnthreadedReviewBodies = append(result.UnthreadedReviewBodies, ReviewBody{ReviewID: review.ID, ReviewNodeID: review.NodeID, URL: review.URL, Author: review.Author.Login, AuthorKind: kind, Bot: bot, Commit: review.CommitID, State: review.State, Label: label, ThreadMatch: match, ParseStatus: parse, Excerpt: excerpt, ExcerptClipped: clipped, Untrusted: true})
	}
	result.Counts.UnthreadedBodies = len(result.UnthreadedReviewBodies)
	source.Items = len(result.UnresolvedThreads) + len(result.UnthreadedReviewBodies)
	sort.Strings(source.Limitations)
	return result, source
}

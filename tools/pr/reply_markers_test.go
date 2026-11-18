package pr

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFindingSeveritySubscriptBadge(t *testing.T) {
	bot := "codex"
	const badge = "![P1 Badge](https://img.shields.io/badge/P1-orange)"
	for _, tc := range []struct {
		body, severity, parseStatus string
	}{
		{"**<sub><sub>" + badge + "</sub></sub> Preserve the header**", "P1", "ok"},
		{"**<sub><sub>![P9 Badge](wrong)</sub></sub>**", "", "unrecognized"},
		{"Review text quotes **<sub><sub>" + badge + "</sub></sub>**", "", "unrecognized"},
	} {
		severity, status := findingSeverity(&bot, tc.body)
		got := ""
		if severity != nil {
			got = *severity
		}
		if got != tc.severity || status != tc.parseStatus {
			t.Errorf("findingSeverity(%q) = %q, %s", tc.body, got, status)
		}
	}
}

func TestFindingRepliesWithTiedTimestamps(t *testing.T) {
	r := Actor{Login: "reviewer", Type: "User"}
	a := Actor{Login: "author", Type: "User"}
	b := Actor{Login: "maintainer", Type: "User"}
	at := func(actor Actor, sec int64) Comment {
		return Comment{Author: actor, CreatedAt: time.Unix(sec, 0)}
	}
	for _, tc := range []struct {
		name                  string
		comments              []Comment
		authorAfter, complete bool
		latest                string
	}{
		{"before tied reviewer followup", []Comment{at(r, 0), at(a, 1), at(r, 1)}, false, false, ""},
		{"after tied reviewer followup", []Comment{at(r, 0), at(r, 1), at(a, 1)}, false, false, ""},
		{"distinct authors share latest timestamp", []Comment{at(r, 0), at(a, 1), at(b, 1)}, true, false, ""},
		{"reverse tie has same result", []Comment{at(r, 0), at(b, 1), at(a, 1)}, true, false, ""},
		{"same author shares latest timestamp", []Comment{at(r, 0), at(a, 1), at(a, 1)}, true, true, "author"},
		{"unique latest after earlier tie", []Comment{at(r, 0), at(a, 1), at(b, 1), at(b, 2)}, true, true, "maintainer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Finding{RepliesComplete: true}
			recordFindingReplies(&f, tc.comments, a)
			gotLatest := ""
			if f.LastReplyBy != nil {
				gotLatest = *f.LastReplyBy
			}
			if f.PRAuthorRepliedAfter != tc.authorAfter || f.RepliesComplete != tc.complete || gotLatest != tc.latest {
				t.Fatalf("author replied = %v, complete = %v, last reply = %q", f.PRAuthorRepliedAfter, f.RepliesComplete, gotLatest)
			}
		})
	}
}

func TestCodeRabbitBodyPreservedWithoutMatchedThread(t *testing.T) {
	for _, match := range []string{"unmatched", "unknown"} {
		t.Run(match, func(t *testing.T) {
			s := findingFixture()
			s.Reviews[1].NodeID = "no-matching-thread"
			if match == "unknown" {
				s.Reviews[1].NodeID = ""
			}
			data, _, err := render(s, 128<<10, 4000)
			if err != nil {
				t.Fatal(err)
			}
			var rendered struct {
				Findings OpenFindings `json:"open_findings"`
			}
			if err := json.Unmarshal(data, &rendered); err != nil {
				t.Fatal(err)
			}
			for _, b := range rendered.Findings.UnthreadedReviewBodies {
				if b.ReviewID != s.Reviews[1].ID {
					continue
				}
				if b.ThreadMatch != match || b.Label != "outside_diff_range" || b.Excerpt != s.Reviews[1].Body || b.ExcerptClipped {
					t.Fatalf("CodeRabbit body = %+v", b)
				}
				return
			}
			t.Fatal("CodeRabbit body missing")
		})
	}
}

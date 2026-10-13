package discover

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixtureOptions() PrepareOptions {
	return PrepareOptions{Provider: "all", Project: "/synthetic/repo", Days: 30, Salt: []byte("synthetic-local-salt-for-tests"), Now: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)}
}
func fixtureRow(session, message string) map[string]any {
	return map[string]any{"provider": "codex", "project": "/synthetic/repo", "session_id": session, "timestamp": "2026-10-12T00:00:00Z", "kind": "user", "message": message, "record": 1}
}
func prepareRows(t *testing.T, rows []map[string]any, opts PrepareOptions) *Bundle {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Prepare(strings.NewReader(string(encoded)), opts)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestDuplicateIdentityVersusIndependentRepeat(t *testing.T) {
	message := "Reconcile orchard irrigation schedules with independent rainfall measurements and valve calibration."
	row := fixtureRow("session-a", message)
	bundle := prepareRows(t, []map[string]any{row, row, fixtureRow("session-b", message)}, fixtureOptions())
	if bundle.Coverage.DuplicateRecords != 1 || len(bundle.Records) != 2 || bundle.Coverage.Sessions != 2 {
		t.Fatalf("unexpected coverage: %+v", bundle.Coverage)
	}
	if len(bundle.Groups) == 0 || bundle.Groups[0].Sessions != 2 {
		t.Fatal("independent recurrence not retained")
	}
	for _, group := range bundle.Groups {
		if strings.Contains(group.Label, "github") {
			t.Fatal("preseeded group")
		}
	}
}

func TestPossibleReplaySequences(t *testing.T) {
	messages := []string{"Calibrate irrigation valves using measured orchard rainfall and moisture gradients before opening the first channel.", "Inspect the orchard drainage channel and compare its moisture measurements against the calibration worksheet.", "Update irrigation scheduling from the observed rainfall and record each valve adjustment in the orchard worksheet."}
	rows := []map[string]any{}
	for session := 0; session < 2; session++ {
		for i, message := range messages {
			row := fixtureRow([]string{"session-a", "session-b"}[session], message)
			row["record"] = i
			row["timestamp"] = time.Date(2026, 10, 11+session, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
			rows = append(rows, row)
		}
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	if bundle.Coverage.PossibleReplayRecords != 3 || bundle.Coverage.Sessions != 2 || len(bundle.Records) != 6 {
		t.Fatalf("unexpected replay coverage: %+v", bundle.Coverage)
	}
	for _, group := range bundle.Groups {
		if group.Sessions != 1 {
			t.Errorf("replay increased sessions: %+v", group)
		}
	}
}

func TestScopeTimeAndInvalidIdentity(t *testing.T) {
	message := "Inspect irrigation calibration measurements against the orchard scheduling worksheet."
	missing := fixtureRow("", message)
	old := fixtureRow("old", message)
	old["timestamp"] = "2020-01-01T00:00:00Z"
	other := fixtureRow("other", message)
	other["project"] = "/synthetic/other"
	bundle := prepareRows(t, []map[string]any{fixtureRow("good", message), missing, old, other}, fixtureOptions())
	if len(bundle.Records) != 1 || bundle.Coverage.RejectedRecords != 3 || bundle.Coverage.Complete {
		t.Fatalf("unexpected scope coverage: %+v", bundle.Coverage)
	}
	opts := fixtureOptions()
	opts.AllProjects = true
	bundle = prepareRows(t, []map[string]any{fixtureRow("good", message), other}, opts)
	if bundle.Coverage.Projects != 2 || !bundle.Coverage.Complete {
		t.Fatalf("all projects: %+v", bundle.Coverage)
	}
}

func TestDeterministicIDsAndSaltIsolation(t *testing.T) {
	rows := []map[string]any{fixtureRow("private-session", "Inspect /home/person/private-project calibration reports and orchard irrigation measurements.")}
	first := prepareRows(t, rows, fixtureOptions())
	second := prepareRows(t, rows, fixtureOptions())
	if first.Records[0].ID != second.Records[0].ID || first.Records[0].Text != second.Records[0].Text {
		t.Fatal("unstable preparation")
	}
	opts := fixtureOptions()
	opts.Salt = []byte("different-installation-salt-value")
	third := prepareRows(t, rows, opts)
	if first.Records[0].ID == third.Records[0].ID || first.Records[0].SessionID == third.Records[0].SessionID {
		t.Fatal("salt not applied")
	}
	encoded, _ := json.Marshal(first)
	for _, private := range []string{"private-session", "private-project", "/home/person", "/synthetic/repo"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("private identity leaked: %q", private)
		}
	}
}

func TestResourceIncidentsAndSafeAliases(t *testing.T) {
	rows := []map[string]any{fixtureRow("session-a", "Inspect irrigation calibration regression at https://github.com/private-owner/private-repo/pull/42?token=secret before changing the orchard schedule."), fixtureRow("session-b", "Compare irrigation calibration regression at https://github.com/private-owner/private-repo/pull/42 against the revised orchard schedule.")}
	bundle := prepareRows(t, rows, fixtureOptions())
	if len(bundle.Records[0].ResourceIDs) != 1 || len(bundle.Records[1].ResourceIDs) != 1 {
		t.Fatalf("missing resource identities: %+v", bundle.Records)
	}
	found := false
	for _, group := range bundle.Groups {
		if group.Label == "resource:github-pr" {
			found = true
			if group.Incidents != 1 || group.Sessions != 2 {
				t.Fatalf("resource grouping: %+v", group)
			}
		}
	}
	if !found {
		t.Fatal("resource lead missing")
	}
	encoded, _ := json.Marshal(bundle)
	if strings.Contains(string(encoded), "private-owner") || strings.Contains(string(encoded), "private-repo") {
		t.Fatal("resource owner leaked")
	}
}

func TestCapsAndMalformedInput(t *testing.T) {
	opts := fixtureOptions()
	opts.MaxRecords = 1
	opts.MaxRecordBytes = 50
	rows := []map[string]any{fixtureRow("session-a", strings.Repeat("calibration orchard irrigation ", 30)), fixtureRow("session-b", "Inspect orchard irrigation calibration measurements.")}
	bundle := prepareRows(t, rows, opts)
	if bundle.Coverage.Complete || !bundle.Records[0].Truncated || len(bundle.Records[0].Text) > 50 {
		t.Fatalf("caps invisible: %+v", bundle.Coverage)
	}
	encoded, _ := json.Marshal(rows[0])
	bundle, err := Prepare(strings.NewReader("["+string(encoded)+",invalid-secret"), fixtureOptions())
	if err != nil || bundle.Coverage.Complete || len(bundle.Records) != 1 {
		t.Fatalf("malformed prefix not described: %v %+v", err, bundle)
	}
	if _, err := Prepare(strings.NewReader("private content"), fixtureOptions()); err == nil || strings.Contains(err.Error(), "private content") {
		t.Fatal("unsafe parse error")
	}
}

func TestNoiseRetainedWithoutTopics(t *testing.T) {
	rows := []map[string]any{fixtureRow("session-a", "Yes, thanks."), fixtureRow("session-b", "```go\nfunc orchard() { return irrigationCalibrationMeasurement }\n```")}
	bundle := prepareRows(t, rows, fixtureOptions())
	if len(bundle.Records) != 2 || len(bundle.Groups) != 0 {
		t.Fatal("noise should remain retrievable without leads")
	}
}

func TestExactCapStillValidatesClosingInput(t *testing.T) {
	row, _ := json.Marshal(fixtureRow("session-a", "Inspect orchard irrigation calibration measurements against scheduling worksheets."))
	opts := fixtureOptions()
	opts.MaxRecords = 1
	for _, suffix := range []string{"] trailing-secret", ""} {
		bundle, err := Prepare(strings.NewReader("["+string(row)+suffix), opts)
		if err != nil || bundle.Coverage.Complete {
			t.Fatalf("bad suffix accepted: %v %+v", err, bundle)
		}
	}
}

func TestFractionalTimestampsAndSameTimeReplay(t *testing.T) {
	a := fixtureRow("session-a", "Inspect orchard irrigation calibration measurements against scheduling worksheets.")
	b := fixtureRow("session-b", "Compare orchard irrigation scheduling against yesterday's calibration measurements.")
	a["timestamp"] = "2026-10-12T00:00:00.1Z"
	bundle := prepareRows(t, []map[string]any{a, b}, fixtureOptions())
	if bundle.Coverage.ObservedSince != "2026-10-12T00:00:00Z" || bundle.Coverage.ObservedUntil != "2026-10-12T00:00:00.1Z" {
		t.Fatalf("timestamp ordering: %+v", bundle.Coverage)
	}
}

func TestLargeRowIsRejectedAndCoverageExplicit(t *testing.T) {
	opts := fixtureOptions()
	rows := []map[string]any{fixtureRow("large", strings.Repeat("x", (2<<20)+1)), fixtureRow("good", "Inspect orchard irrigation calibration measurements against scheduling worksheets.")}
	bundle := prepareRows(t, rows, opts)
	if bundle.Coverage.Complete || bundle.Coverage.RejectedRecords != 1 || len(bundle.Records) != 1 {
		t.Fatalf("oversize row not visible: %+v", bundle.Coverage)
	}
}

func TestReplayWithSameTimestampKeepsSourceSequence(t *testing.T) {
	messages := []string{"Calibrate irrigation valves using measured orchard rainfall and moisture gradients before opening the first channel.", "Inspect orchard drainage channels and compare moisture measurements against the calibration worksheet.", "Update irrigation scheduling from observed rainfall and record every valve adjustment in the orchard worksheet."}
	rows := []map[string]any{}
	for _, session := range []string{"session-a", "session-b"} {
		for i, message := range messages {
			row := fixtureRow(session, message)
			row["record"] = i
			rows = append(rows, row)
		}
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	if bundle.Coverage.PossibleReplayRecords != 3 {
		t.Fatalf("same-time replay not detected: %+v", bundle.Coverage)
	}
}

func TestFallbackCountsSessionsInsteadOfMessages(t *testing.T) {
	rows := []map[string]any{
		fixtureRow("session-a", "Inspect orchard irrigation calibration measurements against scheduling worksheets."),
		fixtureRow("session-a", "Adjust orchard irrigation calibration valves using rainfall measurements."),
		fixtureRow("session-b", "Compare orchard irrigation calibration measurements with yesterday's worksheet."),
		fixtureRow("session-b", "Schedule orchard irrigation calibration checks after drainage maintenance."),
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	for _, group := range bundle.Groups {
		if group.Label == "orchard" {
			if group.Records != 4 || group.Sessions != 2 || group.Incidents != 2 {
				t.Fatalf("fallback counts inflated by messages: %+v", group)
			}
			return
		}
	}
	t.Fatal("expected orchard lead")
}

func TestResourceSessionDoesNotAlsoCountFallback(t *testing.T) {
	rows := []map[string]any{
		fixtureRow("session-a", "Inspect orchard irrigation calibration measurements at https://github.com/synthetic-owner/synthetic-repo/pull/42 before scheduling maintenance."),
		fixtureRow("session-a", "Compare orchard irrigation calibration measurements against yesterday's scheduling worksheet."),
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	for _, group := range bundle.Groups {
		if group.Label == "orchard" {
			if group.Records != 2 || group.Sessions != 1 || group.Incidents != 1 {
				t.Fatalf("session fallback double-counted resource evidence: %+v", group)
			}
			return
		}
	}
	t.Fatal("expected orchard lead")
}

func TestFillerDoesNotCreateLeads(t *testing.T) {
	rows := []map[string]any{
		fixtureRow("session-a", "I don't really like how this does something with stuff because you know, please look."),
		fixtureRow("session-b", "dont like look does something stuff because that's just what I'd really say."),
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	if len(bundle.Groups) != 0 {
		t.Fatalf("filler became lexical leads: %+v", bundle.Groups)
	}
}

func TestShortRequestsAcronymsAndResourceOnlyLeads(t *testing.T) {
	rows := []map[string]any{}
	for _, session := range []string{"session-a", "session-b"} {
		for _, message := range []string{"Review PR", "Repair CI", "Polish UI", "Resize VM", "Irrigation?", "https://github.com/synthetic-owner/synthetic-repo/pull/42"} {
			rows = append(rows, fixtureRow(session, message))
		}
	}
	bundle := prepareRows(t, rows, fixtureOptions())
	labels := map[string]Group{}
	for _, group := range bundle.Groups {
		labels[group.Label] = group
	}
	for _, label := range []string{"pr", "ci", "ui", "vm", "irrigation", "resource:github-pr"} {
		group, ok := labels[label]
		if !ok || group.Sessions != 2 {
			t.Errorf("missing short/resource lead %q: %+v", label, group)
		}
	}
}

func TestConciseIntentSurvivesFencedPaste(t *testing.T) {
	text := "Repair CI for orchard irrigation.\n```go\n" + strings.Repeat("ExtraordinaryImplementationIdentifier", 40) + "\n```"
	terms := topicTerms(text)
	if strings.Join(terms, " ") != "repair ci orchard irrigation" {
		t.Fatalf("pasted code displaced intent: %v", terms)
	}
}

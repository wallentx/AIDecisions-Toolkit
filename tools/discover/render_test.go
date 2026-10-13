package discover

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func renderFixture(count int) *Bundle {
	b := &Bundle{SchemaVersion: 1}
	for i := range count {
		id := fmt.Sprintf("record-%03d", i)
		b.Records = append(b.Records, Record{ID: id, SessionID: fmt.Sprintf("session-%d", i), Text: strings.Repeat("Long synthetic evidence with quoted \"values\" and unicode λ. ", 70)})
		b.Groups = append(b.Groups, Group{ID: fmt.Sprintf("group-%d", i), Label: "calibration", Terms: []string{"calibration"}, RecordIDs: []string{id}, Sessions: 2, Records: 1})
	}
	return b
}

func renderForTest(t *testing.T, bundle *Bundle, opts renderOptions) report {
	t.Helper()
	data, err := renderBundle(bundle, CacheInfo{ID: strings.Repeat("a", 64)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > opts.MaxBytes {
		t.Fatalf("size=%d exceeds max=%d", len(data), opts.MaxBytes)
	}
	var got report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRenderBudgetPaginationNeverSkipsResults(t *testing.T) {
	for _, records := range []bool{true, false} {
		t.Run(fmt.Sprintf("records=%t", records), func(t *testing.T) {
			bundle := renderFixture(17)
			opts := renderOptions{PageSize: 17, MinSessions: 1, TextLimit: 1600, MaxBytes: 4096, AllRecords: records}
			var gotIDs, wantIDs []string
			for _, record := range bundle.Records {
				wantIDs = append(wantIDs, record.ID)
			}
			if !records {
				wantIDs = nil
				for _, group := range bundle.Groups {
					wantIDs = append(wantIDs, group.ID)
				}
			}
			for pages := 0; ; pages++ {
				if pages > len(wantIDs) {
					t.Fatal("pagination did not terminate")
				}
				got := renderForTest(t, bundle, opts)
				if got.Page.Returned == 0 {
					t.Fatal("nonempty page made no progress")
				}
				if pages == 0 && got.Page.Returned >= 17 {
					t.Fatal("fixture did not force budget trimming")
				}
				for _, item := range got.Evidence {
					gotIDs = append(gotIDs, item.ID)
				}
				for _, item := range got.Leads {
					gotIDs = append(gotIDs, item.ID)
				}
				if got.Page.NextOffset == nil {
					break
				}
				if *got.Page.NextOffset != opts.Offset+got.Page.Returned {
					t.Fatal("next offset skipped or repeated results")
				}
				opts.Offset = *got.Page.NextOffset
			}
			if !reflect.DeepEqual(gotIDs, wantIDs) {
				t.Fatalf("items lost or repeated: got=%v want=%v", gotIDs, wantIDs)
			}
		})
	}
}

func TestRenderTextPagingPreservesUnicodeAndBudget(t *testing.T) {
	bundle := renderFixture(1)
	opts := renderOptions{PageSize: 1, MinSessions: 1, TextLimit: 12000, MaxBytes: 4096, Evidence: bundle.Records[0].ID}
	var combined strings.Builder
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("text continuation did not terminate")
		}
		got := renderForTest(t, bundle, opts)
		item := got.Evidence[0]
		combined.WriteString(item.Text)
		if item.NextTextOffset == nil {
			break
		}
		if *item.NextTextOffset <= opts.TextOffset || *item.NextTextOffset != opts.TextOffset+len([]rune(item.Text)) {
			t.Fatal("text continuation did not advance by characters")
		}
		opts.TextOffset = *item.NextTextOffset
	}
	if combined.String() != bundle.Records[0].Text {
		t.Fatal("text pagination corrupted or dropped content")
	}
}

func TestRenderRejectsInvalidEvidenceAndBounds(t *testing.T) {
	bundle := renderFixture(1)
	base := renderOptions{PageSize: 1, MinSessions: 1, TextLimit: 10, MaxBytes: 4096}
	cases := []renderOptions{base, base, base, base, base, base}
	cases[0].Evidence = "unknown-id"
	cases[1].Offset = 2
	cases[2].Evidence, cases[2].TextOffset = bundle.Groups[0].ID, 1
	cases[3].AllRecords, cases[3].TextOffset = true, 1
	cases[4].Evidence, cases[4].TextOffset = bundle.Records[0].ID, len([]rune(bundle.Records[0].Text))+1
	cases[5].PageSize = 0
	for i, opts := range cases {
		if _, err := renderBundle(bundle, CacheInfo{}, opts); err == nil {
			t.Errorf("case %d accepted invalid bounds", i)
		}
	}
}

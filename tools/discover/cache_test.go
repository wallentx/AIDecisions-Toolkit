package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheRefusesSymlinksAndAtomicFailurePreservesExisting(t *testing.T) {
	parent := canonicalTemp(t)
	target := filepath.Join(parent, "real")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if err := ensurePrivateDir(filepath.Join(link, "nested")); err == nil {
		t.Fatal("accepted symlink parent")
	}
	dir := filepath.Join(parent, "cache")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "existing")
	if err := atomicPrivateWrite(dir, "existing", []byte("safe existing snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, filepath.Join(dir, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := atomicPrivateWrite(dir, "redirect", []byte("replacement")); err == nil {
		t.Fatal("accepted symlink target")
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "safe existing snapshot" {
		t.Fatalf("failed write changed original: %q %v", data, err)
	}
	if _, err := readPrivateFile(dir, "redirect", 1024); err == nil {
		t.Fatal("read symlink target")
	}
	if err := os.Mkdir(filepath.Join(dir, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := atomicPrivateWrite(dir, "blocked", []byte("replacement")); err == nil {
		t.Fatal("replaced directory target")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			t.Fatal("temporary file survived failure")
		}
	}
}
func TestCacheLockCancellationAndSnapshotValidation(t *testing.T) {
	dir := filepath.Join(canonicalTemp(t), "cache")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCache(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = lockCache(ctx, dir); err == nil {
		t.Fatal("concurrent lock unexpectedly succeeded")
	}
	unlock()
	unlock, err = lockCache(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err = ReadSnapshot(dir, "../identity-salt"); err == nil {
		t.Fatal("accepted snapshot path traversal")
	}
}
func TestMalformedRefreshDoesNotReplaceSnapshot(t *testing.T) {
	options := syntheticOptions(t)
	raw := syntheticSource(t, "Review recurring deployment failures and document successful remediation steps.")
	original, info, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	options.Refresh = true
	if _, _, err = Load(context.Background(), options, strings.NewReader(`[{"message":"secret=do-not-cache"}`)); err == nil {
		t.Fatal("malformed source accepted")
	}
	snapshot, err := ReadSnapshot(options.CacheDir, info.ID)
	if err != nil || len(snapshot.Records) != len(original.Records) {
		t.Fatalf("old snapshot lost: %v", err)
	}
	entries, _ := os.ReadDir(options.CacheDir)
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(options.CacheDir, entry.Name()))
		if strings.Contains(string(data), "do-not-cache") {
			t.Fatal("malformed raw input cached")
		}
	}
}

func TestSnapshotRejectsTamperedBundle(t *testing.T) {
	options := syntheticOptions(t)
	raw := syntheticSource(t, "Review repeated deployment failures and validate the release process carefully.")
	_, info, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	name := "snapshot-" + info.ID + ".json"
	data, err := readPrivateFile(options.CacheDir, name, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var entry cacheEntry
	if err = json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if len(entry.Bundle.Records) != 1 {
		t.Fatalf("expected one prepared record, got %d", len(entry.Bundle.Records))
	}
	entry.Bundle.Records[0].Text = "Tampered evidence never emitted by preparation"
	data, err = json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicPrivateWrite(options.CacheDir, name, data); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSnapshot(options.CacheDir, info.ID); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
}

func TestSnapshotRetentionAndProvenance(t *testing.T) {
	options := syntheticOptions(t)
	options.Refresh = true
	raw := syntheticSource(t, "Review orchard irrigation reports and summarize recurring calibration failures.")
	var latest CacheInfo
	for i := 0; i < 10; i++ {
		_, info, err := Load(context.Background(), options, strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		latest = info
	}
	entries, err := os.ReadDir(options.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, keys := 0, 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "snapshot-") {
			snapshots++
		}
		if strings.HasPrefix(entry.Name(), "key-") {
			keys++
			stat, _ := entry.Info()
			if stat.Size() > 4096 {
				t.Fatal("key duplicates snapshot payload")
			}
		}
	}
	if snapshots != 8 || keys != 1 {
		t.Fatalf("retention snapshots=%d keys=%d", snapshots, keys)
	}
	_, info, err := ReadSnapshotInfo(options.CacheDir, latest.ID)
	if err != nil || info.Source != latest.Source || info.SourceID != latest.SourceID || info.Provider != "codex" {
		t.Fatalf("snapshot provenance: %+v %v", info, err)
	}
}

func TestRetentionLeavesUnrecognizedFilesAlone(t *testing.T) {
	dir := filepath.Join(canonicalTemp(t), "cache")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, digit := range "0123456789" {
		name := "snapshot-" + strings.Repeat(string(digit), 64) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"unrelated":true}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneCache(dir, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 10 {
		t.Fatalf("retention removed unrecognized files: %d %v", len(entries), err)
	}
}

func TestCacheRejectsSwappedScopeReferences(t *testing.T) {
	options := syntheticOptions(t)
	raw := syntheticSource(t, "Review orchard irrigation reports and summarize recurring calibration failures.")
	_, first, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	other := options
	other.Provider = "all"
	_, second, err := Load(context.Background(), other, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := readPrivateFile(options.CacheDir, "key-"+second.SourceID+".json", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicPrivateWrite(options.CacheDir, "key-"+first.SourceID+".json", reference); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Load(context.Background(), options, strings.NewReader(raw))
	if err == nil || bundle != nil {
		t.Fatal("swapped reference returned another provider scope")
	}
}

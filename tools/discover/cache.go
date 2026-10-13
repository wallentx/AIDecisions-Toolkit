package discover

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

type cacheEntry struct {
	Version   string    `json:"cache_version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"as_of"`
	Bundle    Bundle    `json:"bundle"`
	Info      CacheInfo `json:"source_info"`
}

var snapshotIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ReadSnapshot retrieves only a previously sanitized snapshot, without scanning
// history or invoking Reflect. Snapshot identifiers are opaque hexadecimal IDs.
func ReadSnapshot(cacheDir, id string) (*Bundle, error) {
	bundle, _, err := ReadSnapshotInfo(cacheDir, id)
	return bundle, err
}

func ReadSnapshotInfo(cacheDir, id string) (*Bundle, CacheInfo, error) {
	if !snapshotIDPattern.MatchString(id) {
		return nil, CacheInfo{}, errors.New("invalid discover snapshot ID")
	}
	if cacheDir == "" {
		var err error
		cacheDir, err = DefaultCacheDir()
		if err != nil {
			return nil, CacheInfo{}, err
		}
	}
	entry, err := readCacheEntry(cacheDir, "snapshot-"+id+".json")
	if err != nil {
		return nil, CacheInfo{}, err
	}
	if entry.ID != id {
		return nil, CacheInfo{}, errors.New("discover snapshot identity mismatch")
	}
	info := entry.Info
	info.Hit = true
	info.AgeSeconds = int64(time.Since(entry.CreatedAt).Seconds())
	return &entry.Bundle, info, nil
}

func readCacheEntry(dir, name string) (*cacheEntry, error) {
	data, err := readPrivateFile(dir, name, maxCacheEntryBytes)
	if err != nil {
		return nil, err
	}
	var entry cacheEntry
	if err = json.Unmarshal(data, &entry); err != nil {
		return nil, errors.New("discover cache entry is malformed; use --refresh to rebuild it")
	}
	if entry.Version != sourceCacheVersion || !snapshotIDPattern.MatchString(entry.ID) || entry.CreatedAt.IsZero() || entry.CreatedAt.UTC().Format(time.RFC3339Nano) != entry.Bundle.PreparedAt {
		return nil, errors.New("discover cache entry has an incompatible schema; use --refresh to rebuild it")
	}
	if entry.Info.ID != entry.ID || entry.Info.AsOf != entry.Bundle.PreparedAt || entry.Info.SourceID == "" || entry.Info.Provider == "" {
		return nil, errors.New("discover cached provenance is inconsistent")
	}
	if err := validateCachedBundle(&entry.Bundle); err != nil {
		return nil, err
	}
	salt, err := readPrivateFile(dir, "identity-salt", 32)
	if err != nil || len(salt) != 32 {
		return nil, errors.New("discover cache identity salt is missing or invalid")
	}
	prepared, err := json.Marshal(&entry.Bundle)
	if err != nil {
		return nil, errors.New("discover cached bundle cannot be encoded")
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(prepared)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(entry.ID)) {
		return nil, errors.New("discover cache snapshot integrity check failed")
	}
	return &entry, nil
}

func ensurePrivateDir(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return errors.New("cannot resolve discover cache directory")
	}
	_, leafErr := os.Lstat(absolute)
	created := errors.Is(leafErr, os.ErrNotExist)
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err = os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return errors.New("cannot create private discover cache directory")
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("discover cache path must contain only real directories, without symlinks")
		}
	}
	// The leaf directory contains sensitive sanitized evidence and the identity salt.
	return secureCacheDirectory(absolute, created)
}

func checkCachePath(dir, name string) (string, error) {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return "", errors.New("invalid discover cache filename")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", errors.New("cannot resolve discover cache path")
	}
	current := filepath.VolumeName(absolute) + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return "", os.ErrNotExist
			}
			return "", errors.New("cannot inspect discover cache path")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("discover cache path must contain only real directories, without symlinks")
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", errors.New("cannot inspect discover cache directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", errors.New("discover cache directory is not private")
	}
	if err := verifyCachePrivacy(absolute); err != nil {
		return "", err
	}
	path := filepath.Join(absolute, name)
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", errors.New("cannot inspect discover cache target")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("discover cache target must be a regular file without symlinks")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", errors.New("discover cache file is not private")
	}
	if err := verifyCachePrivacy(path); err != nil {
		return "", err
	}
	return path, nil
}

func readPrivateFile(dir, name string, limit int64) ([]byte, error) {
	path, err := checkCachePath(dir, name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("cannot read private discover cache file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("discover cache file is unreadable or exceeds its size limit")
	}
	return data, nil
}

func persistentSalt(dir string) ([]byte, error) {
	salt, err := readPrivateFile(dir, "identity-salt", 32)
	if err == nil {
		if len(salt) != 32 {
			return nil, errors.New("discover identity salt is malformed")
		}
		return salt, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	salt = make([]byte, 32)
	if _, err = rand.Read(salt); err != nil {
		return nil, errors.New("cannot generate discover identity salt")
	}
	if err = atomicPrivateWrite(dir, "identity-salt", salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// lockCache serializes salt creation and replacement. An interrupted owner can
// leave a lock directory; bounded waits fail explicitly instead of breaking a
// possibly live writer's lock. No source content is stored in the lock.
func lockCache(ctx context.Context, dir string) (func(), error) {
	lock := filepath.Join(dir, ".lock")
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		if err := wait.Err(); err != nil {
			return nil, errors.New("discover cache is busy or the operation was canceled")
		}
		err := os.Mkdir(lock, 0700)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, errors.New("cannot lock discover cache")
		}
		info, err := os.Lstat(lock)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("discover cache lock is unsafe")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return nil, errors.New("discover cache is busy or the operation was canceled")
		case <-timer.C:
		}
	}
}

func atomicPrivateWrite(dir, name string, data []byte) error {
	if len(data) > maxCacheEntryBytes {
		return errors.New("discover cache entry exceeds its size limit")
	}
	target, err := checkCachePath(dir, name)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return errors.New("cannot create private discover cache temporary file")
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return errors.New("cannot secure discover cache temporary file")
	}
	if _, err = file.Write(data); err != nil {
		return errors.New("cannot write discover cache temporary file")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot sync discover cache temporary file")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close discover cache temporary file")
	}
	success = true
	// Recheck immediately before replacement, including every parent component.
	if _, err = checkCachePath(dir, name); err != nil {
		return err
	}
	if err = os.Rename(tmp, target); err != nil {
		return errors.New("cannot atomically replace discover cache entry")
	}
	// Windows uses the per-user cache directory ACL; POSIX modes and directory
	// fsync are unavailable there. The file itself was synced before replacement.
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return errors.New("cannot open discover cache directory for sync")
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return errors.New("cannot sync discover cache directory")
	}
	return nil
}

func validateCachedBundle(bundle *Bundle) error {
	invalid := func() error { return errors.New("discover cached evidence has inconsistent schema or references") }
	if bundle.SchemaVersion != SchemaVersion || bundle.Coverage.RetainedRecords != len(bundle.Records) {
		return invalid()
	}
	if _, err := time.Parse(time.RFC3339Nano, bundle.PreparedAt); err != nil {
		return invalid()
	}
	records := make(map[string]bool, len(bundle.Records))
	sessions, projects := map[string]bool{}, map[string]bool{}
	for _, record := range bundle.Records {
		if record.ID == "" || records[record.ID] || record.SessionID == "" || record.ProjectID == "" {
			return invalid()
		}
		records[record.ID] = true
		sessions[record.SessionID] = true
		projects[record.ProjectID] = true
	}
	if bundle.Coverage.Sessions != len(sessions) || bundle.Coverage.Projects != len(projects) {
		return invalid()
	}
	for _, record := range bundle.Records {
		if record.ReplayOf != "" && !records[record.ReplayOf] {
			return invalid()
		}
	}
	groups := make(map[string]bool, len(bundle.Groups))
	for _, group := range bundle.Groups {
		if group.ID == "" || groups[group.ID] || group.Records != len(group.RecordIDs) {
			return invalid()
		}
		groups[group.ID] = true
		seen := make(map[string]bool, len(group.RecordIDs))
		for _, id := range group.RecordIDs {
			if !records[id] || seen[id] {
				return invalid()
			}
			seen[id] = true
		}
	}
	return nil
}

const maxCacheEntryBytes = 128 << 20
const maxCacheSnapshots = 8
const maxCacheTotalBytes int64 = 256 << 20

type cacheReference struct {
	Version string `json:"cache_version"`
	ID      string `json:"snapshot_id"`
}

func readCacheKey(dir, key string) (*cacheEntry, error) {
	data, err := readPrivateFile(dir, "key-"+key+".json", 4096)
	if err != nil {
		return nil, err
	}
	var ref cacheReference
	if json.Unmarshal(data, &ref) != nil || ref.Version != sourceCacheVersion || !snapshotIDPattern.MatchString(ref.ID) {
		return nil, errors.New("discover cache reference is invalid; use --refresh")
	}
	entry, err := readCacheEntry(dir, "snapshot-"+ref.ID+".json")
	if err != nil {
		return nil, err
	}
	if entry.Info.SourceID != key {
		return nil, errors.New("discover cache reference does not match the requested source scope")
	}
	return entry, nil
}

// Retain the newest eight snapshots within 256 MiB. Only recognized regular
// cache files are candidates; unrelated entries and symlinks are left alone.
func pruneCache(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("cannot inspect discover cache retention")
	}
	type candidate struct {
		name, id string
		size     int64
		stamp    time.Time
	}
	var snapshots []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "snapshot-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "snapshot-"), ".json")
		stat, err := entry.Info()
		if err != nil || !stat.Mode().IsRegular() || !snapshotIDPattern.MatchString(id) {
			continue
		}
		owned, err := readCacheEntry(dir, name)
		if err != nil || owned.ID != id {
			continue
		}
		snapshots = append(snapshots, candidate{name, id, stat.Size(), stat.ModTime()})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].id == keep {
			return true
		}
		if snapshots[j].id == keep {
			return false
		}
		return snapshots[i].stamp.After(snapshots[j].stamp)
	})
	retained := map[string]bool{}
	var total int64
	for _, entry := range snapshots {
		if len(retained) < maxCacheSnapshots && total+entry.size <= maxCacheTotalBytes {
			retained[entry.id] = true
			total += entry.size
			continue
		}
		// Refuse to remove an unrecognized or corrupted file even if its name
		// resembles a cache-owned snapshot.
		owned, inspectErr := readCacheEntry(dir, entry.name)
		if inspectErr != nil || owned.ID != entry.id {
			continue
		}
		path, err := checkCachePath(dir, entry.name)
		if err != nil {
			return err
		}
		if err = os.Remove(path); err != nil {
			return errors.New("cannot expire discover snapshot")
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "key-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(name, "key-"), ".json")
		stat, err := entry.Info()
		if err != nil || !stat.Mode().IsRegular() || !snapshotIDPattern.MatchString(key) {
			continue
		}
		data, err := readPrivateFile(dir, name, 4096)
		if err != nil {
			continue
		}
		var ref cacheReference
		if json.Unmarshal(data, &ref) != nil || ref.Version != sourceCacheVersion || !snapshotIDPattern.MatchString(ref.ID) {
			continue
		}
		if !retained[ref.ID] {
			path, err := checkCachePath(dir, name)
			if err != nil {
				return err
			}
			if os.Remove(path) != nil {
				return errors.New("cannot expire discover cache reference")
			}
		}
	}
	return nil
}

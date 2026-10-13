package discover

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aid/internal/buildinfo"
)

const sourceCacheVersion = "2"
const defaultMaxInputBytes int64 = 64 << 20

// SourceOptions controls normalized JSON input or an explicit Reflect scan.
type SourceOptions struct {
	Input, Reflect, Provider, Project, CacheDir string
	Days                                        int
	AllProjects, Refresh, NoCache               bool
	CacheTTL, Timeout                           time.Duration
	MaxInputBytes                               int64
	MaxRecords, MaxRecordBytes                  int
}

// CacheInfo describes freshness without disclosing private source paths.
type CacheInfo struct {
	Hit          bool     `json:"hit"`
	ID           string   `json:"id,omitempty"`
	AsOf         string   `json:"as_of"`
	AgeSeconds   int64    `json:"age_seconds"`
	Source       string   `json:"source"`
	SourceID     string   `json:"source_id,omitempty"`
	SourceSchema string   `json:"source_schema"`
	Provider     string   `json:"provider"`
	Days         int      `json:"days"`
	AllProjects  bool     `json:"all_projects"`
	AdapterFlags []string `json:"adapter_flags,omitempty"`
	IDStability  string   `json:"id_stability"`
	Warnings     []string `json:"warnings,omitempty"`
}

// DefaultCacheDir returns the per-user private discover cache location.
func DefaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", errors.New("cannot determine discover cache directory")
	}
	// Canonicalize an existing OS-provided cache root (for example macOS /var).
	if canonical, err := filepath.EvalSymlinks(dir); err == nil {
		dir = canonical
	}
	return filepath.Join(dir, "AID", "discover"), nil
}

// Load prepares a sanitized snapshot. Raw source bytes are bounded in memory and
// are never written to the cache. Reflect caching is TTL based, not incremental.
func Load(ctx context.Context, options SourceOptions, stdin io.Reader) (*Bundle, CacheInfo, error) {
	options, err := normalizeSourceOptions(options)
	if err != nil {
		return nil, CacheInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	now := time.Now().UTC()
	info := CacheInfo{Source: "reflect", IDStability: "persistent", SourceSchema: "reflect.normalized-array.v1", Provider: options.Provider, Days: options.Days, AllProjects: options.AllProjects}
	var raw []byte
	var identity string
	var command *exec.Cmd
	if options.Input != "" {
		info.Source = "file"
		var reader io.Reader
		var file *os.File
		if options.Input == "-" {
			info.Source = "stdin"
			if stdin == nil {
				return nil, info, errors.New("standard input is unavailable")
			}
			reader = stdin
		} else {
			stat, statErr := os.Stat(options.Input)
			if statErr != nil || !stat.Mode().IsRegular() {
				return nil, info, errors.New("discover input must be a regular JSON file or stdin")
			}
			file, err = os.Open(options.Input)
			if err != nil {
				return nil, info, errors.New("cannot open discover input")
			}
			defer file.Close()
			reader = file
		}
		raw, err = readBoundedContext(ctx, reader, options.MaxInputBytes)
		if err != nil {
			return nil, info, err
		}
		digest := sha256.Sum256(raw)
		identity = info.Source + ":" + hex.EncodeToString(digest[:])
	} else {
		info.AdapterFlags = []string{"include-tool-errors", "format=json", "semantic=false"}
		command, identity, err = reflectCommand(ctx, options)
		if err != nil {
			return nil, info, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, info, errors.New("discover source operation canceled or timed out")
	}
	var salt []byte
	var unlock func()
	if options.NoCache {
		salt = make([]byte, 32)
		if _, err = rand.Read(salt); err != nil {
			return nil, info, errors.New("cannot create private record identity salt")
		}
		info.IDStability = "ephemeral"
		info.Warnings = append(info.Warnings, "Caching is disabled; opaque record IDs are stable only within this run.")
	} else {
		if err = ensurePrivateDir(options.CacheDir); err != nil {
			return nil, info, err
		}
		unlock, err = lockCache(ctx, options.CacheDir)
		if err != nil {
			return nil, info, err
		}
		defer unlock()
		salt, err = persistentSalt(options.CacheDir)
		if err != nil {
			return nil, info, err
		}
	}
	key, err := sourceKey(identity, options, salt)
	if err != nil {
		return nil, info, err
	}
	info.SourceID = key
	if !options.NoCache && !options.Refresh {
		cached, readErr := readCacheKey(options.CacheDir, key)
		if readErr == nil {
			age := now.Sub(cached.CreatedAt)
			if age >= 0 && age < options.CacheTTL {
				info.Hit, info.ID = true, cached.ID
				info.AsOf, info.AgeSeconds = cached.CreatedAt.Format(time.RFC3339Nano), int64(age.Seconds())
				return &cached.Bundle, info, nil
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			// Corrupt or insecure existing entries must not silently become valid hits.
			return nil, info, readErr
		}
	}
	if command != nil {
		stdout := &boundedBuffer{max: options.MaxInputBytes}
		stderr := &boundedBuffer{max: 4096, discardOverflow: true}
		command.Stdout, command.Stderr = stdout, stderr
		if runErr := command.Run(); runErr != nil {
			if ctx.Err() != nil {
				return nil, info, errors.New("Reflect scan canceled or timed out")
			}
			if stdout.exceeded {
				return nil, info, errors.New("Reflect output exceeds the input size limit")
			}
			// Never quote helper output: even malformed diagnostics can contain secrets.
			return nil, info, errors.New("Reflect scan failed; helper diagnostics omitted for privacy")
		}
		if stdout.exceeded {
			return nil, info, errors.New("Reflect output exceeds the input size limit")
		}
		raw = stdout.Bytes()
	}
	if !json.Valid(raw) {
		return nil, info, errors.New("invalid normalized Reflect input: malformed JSON")
	}
	bundle, err := Prepare(bytes.NewReader(raw), PrepareOptions{
		Provider: options.Provider, Project: options.Project, Days: options.Days,
		AllProjects: options.AllProjects, Salt: salt, Now: now,
		MaxRecords: options.MaxRecords, MaxRecordBytes: options.MaxRecordBytes,
	})
	if err != nil {
		return nil, info, errors.New("invalid normalized Reflect input: preparation failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, info, errors.New("discover source operation canceled or timed out")
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, info, errors.New("cannot encode prepared discover snapshot")
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(encoded)
	info.ID = hex.EncodeToString(mac.Sum(nil))
	info.AsOf = now.Format(time.RFC3339Nano)
	if !options.NoCache {
		entry := cacheEntry{Version: sourceCacheVersion, ID: info.ID, CreatedAt: now, Bundle: *bundle, Info: info}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, info, errors.New("cannot encode discover cache entry")
		}
		if len(data) > maxCacheEntryBytes {
			return nil, info, errors.New("prepared snapshot exceeds the cache size limit")
		}
		if err = atomicPrivateWrite(options.CacheDir, "snapshot-"+info.ID+".json", data); err != nil {
			return nil, info, err
		}
		reference, _ := json.Marshal(cacheReference{Version: sourceCacheVersion, ID: info.ID})
		if err = atomicPrivateWrite(options.CacheDir, "key-"+key+".json", reference); err != nil {
			return nil, info, err
		}
		if err = pruneCache(options.CacheDir, info.ID); err != nil {
			return nil, info, err
		}
	}
	return bundle, info, nil
}

func normalizeSourceOptions(o SourceOptions) (SourceOptions, error) {
	if o.Provider == "" {
		o.Provider = "all"
	}
	if !labelPattern.MatchString(o.Provider) || Redact(o.Provider) != o.Provider {
		return o, errors.New("provider must be a safe non-sensitive label")
	}
	if o.Days == 0 {
		o.Days = 30
	}
	if o.MaxRecords < 0 || o.MaxRecordBytes < 0 {
		return o, errors.New("preparation limits must be nonnegative")
	}
	if o.Days < 0 {
		return o, errors.New("days must be positive")
	}
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.Timeout < 0 {
		return o, errors.New("timeout must be positive")
	}
	if o.CacheTTL < 0 {
		return o, errors.New("cache TTL must be nonnegative")
	}
	if o.MaxInputBytes == 0 {
		o.MaxInputBytes = defaultMaxInputBytes
	}
	if o.MaxInputBytes < 1 {
		return o, errors.New("input byte limit must be positive")
	}
	if o.Project == "" {
		o.Project = "."
	}
	project, err := filepath.Abs(o.Project)
	if err != nil {
		return o, errors.New("cannot resolve project directory")
	}
	o.Project = filepath.Clean(project)
	if o.CacheDir == "" && !o.NoCache {
		o.CacheDir, err = DefaultCacheDir()
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

func sourceKey(identity string, o SourceOptions, salt []byte) (string, error) {
	toolVersion, err := version()
	if err != nil {
		return "", errors.New("cannot identify discover tool version")
	}
	payload := struct {
		Identity, Provider, Project, SourceSchema, CacheVersion, ToolVersion, ReleaseVersion string
		Days, MaxRecords, MaxRecordBytes, PreparedSchema                                     int
		AllProjects                                                                          bool
		MaxInputBytes                                                                        int64
	}{identity, o.Provider, o.Project, "reflect.normalized-array.v1", sourceCacheVersion, toolVersion, buildinfo.Version, o.Days, o.MaxRecords, o.MaxRecordBytes, SchemaVersion, o.AllProjects, o.MaxInputBytes}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("cannot encode discover source identity")
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func reflectCommand(ctx context.Context, o SourceOptions) (*exec.Cmd, string, error) {
	helper := o.Reflect
	if helper == "" {
		helper = "reflect"
	}
	resolved := helper
	var err error
	// Explicit Python scripts need not carry executable permission.
	if !(o.Reflect != "" && strings.EqualFold(filepath.Ext(helper), ".py")) {
		resolved, err = exec.LookPath(helper)
		if err != nil {
			return nil, "", errors.New("Reflect helper not found; install reflect on PATH or provide --reflect")
		}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, "", errors.New("cannot resolve Reflect helper")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errors.New("Reflect helper is not a readable regular file")
	}
	args := []string{"--provider", o.Provider, "scan", "--project", o.Project, "--days", strconv.Itoa(o.Days)}
	if o.AllProjects {
		args = append(args, "--all-projects")
	}
	args = append(args, "--include-tool-errors", "--format", "json")
	executable := resolved
	if strings.EqualFold(filepath.Ext(resolved), ".py") {
		executable, err = exec.LookPath("python3")
		if err != nil {
			executable, err = exec.LookPath("python")
		}
		if err != nil {
			return nil, "", errors.New("Python is required for the explicit Reflect script")
		}
		args = append([]string{resolved}, args...)
	}
	identity := fmt.Sprintf("%s|%s|%d|%d|%s", executable, resolved, info.Size(), info.ModTime().UnixNano(), strings.Join(args, "\x00"))
	command := exec.CommandContext(ctx, executable, args...)
	command.WaitDelay = time.Second
	return command, identity, nil
}

func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, errors.New("cannot read discover input")
	}
	if int64(len(data)) > max {
		return nil, errors.New("discover input exceeds the input size limit")
	}
	return data, nil
}

type boundedBuffer struct {
	bytes.Buffer
	max                       int64
	exceeded, discardOverflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	count := len(p)
	remaining := b.max - int64(b.Len())
	if int64(count) <= remaining {
		return b.Buffer.Write(p)
	}
	b.exceeded = true
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:remaining])
	}
	if b.discardOverflow {
		return count, nil
	}
	return int(remaining), errors.New("source output size limit exceeded")
}

// ReadCloser inputs are closed when cancellation interrupts a blocking read.
// For arbitrary Readers the result is abandoned; their owner must unblock Read.
func readBoundedContext(ctx context.Context, reader io.Reader, max int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { data, err := readBounded(reader, max); done <- result{data, err} }()
	select {
	case value := <-done:
		return value.data, value.err
	case <-ctx.Done():
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, errors.New("discover input read canceled or timed out")
	}
}

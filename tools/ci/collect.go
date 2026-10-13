package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"aid/internal/evidence"
	"aid/internal/github"
)

var ErrJobNotInAttempt = errors.New("job is not part of the selected workflow run attempt")

// Collect reads one pinned workflow run attempt without mutating GitHub. It
// returns partial evidence with explicit source errors when a request or bound
// prevents complete collection.
func Collect(ctx context.Context, api API, requested github.WorkflowRun, opts Options) (Snapshot, error) {
	opts = normalizeOptions(opts)
	started := time.Now().UTC()
	selection := Selection{Scope: "run_attempt", Description: "all jobs in the pinned workflow run attempt"}
	if requested.JobID != 0 {
		selection = Selection{Scope: "job", JobID: requested.JobID, Description: "one job verified against the pinned workflow run attempt"}
	}
	c := collector{
		ctx:  ctx,
		api:  api,
		opts: opts,
		snapshot: Snapshot{
			SchemaVersion: SchemaVersion,
			Reference:     requested,
			Selection:     selection,
			Provenance: Provenance{
				AttemptSelection: "explicit",
				PinnedAttempt:    requested.Attempt,
				ReadOnly:         true,
				Atomic:           false,
				Limitation:       "GitHub sources are fetched separately and do not form an atomic snapshot.",
			},
			StartedAt:   started,
			Sources:     make(map[string]evidence.Source, len(sourceKeys)),
			Jobs:        []Job{},
			Annotations: []Annotation{},
			Artifacts: ArtifactCollection{
				Scope:              "workflow_run_all_attempts",
				AttemptAttribution: false,
				Limitation:         "GitHub artifact metadata is run-wide and cannot be attributed to the selected attempt.",
				Items:              []Artifact{},
			},
			Output: OutputState{OmittedItems: map[string]int{}},
		},
	}
	for _, key := range sourceKeys {
		c.snapshot.Sources[key] = evidence.Source{}
	}

	ref, ok := c.pinAttempt(requested)
	if !ok {
		c.failDependents("workflow run attempt could not be resolved")
		c.finish()
		return c.snapshot, nil
	}
	c.snapshot.Reference = ref
	c.snapshot.Provenance.PinnedAttempt = ref.Attempt

	start, ok := c.readRun(ref, false)
	if !ok {
		c.failDependents("pinned workflow run identity was unavailable")
		c.finish()
		return c.snapshot, nil
	}
	c.start = identityOf(start)
	c.snapshot.Run = start

	allJobs, jobsComplete := c.collectJobs(ref)
	selected, found := selectJobs(allJobs, ref.JobID)
	if ref.JobID != 0 && !found {
		if jobsComplete {
			c.readRun(ref, true)
			c.finishFreshness(ref)
			c.finish()
			return c.snapshot, ErrJobNotInAttempt
		}
		c.fail(SourceAnnotations, "dependency_failed", "job membership could not be verified from an incomplete job list")
		c.fail(SourceLogs, "dependency_failed", "job membership could not be verified from an incomplete job list")
	} else {
		c.snapshot.Jobs = selected
		c.collectAnnotations(ref)
		c.collectLogs(ref)
	}
	c.collectArtifacts(ref)
	c.readRun(ref, true)
	c.finishFreshness(ref)
	c.finish()
	return c.snapshot, nil
}

type collector struct {
	ctx           context.Context
	api           API
	opts          Options
	snapshot      Snapshot
	start         RunIdentity
	end           RunIdentity
	endOK         bool
	requests      int
	retainedBytes int
}

func normalizeOptions(opts Options) Options {
	defaults := DefaultOptions()
	if opts.PerPage <= 0 {
		opts.PerPage = defaults.PerPage
	}
	if opts.PerPage > 100 {
		opts.PerPage = 100
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaults.MaxPages
	}
	if opts.MaxJobs <= 0 {
		opts.MaxJobs = defaults.MaxJobs
	}
	if opts.MaxLogJobs <= 0 {
		opts.MaxLogJobs = defaults.MaxLogJobs
	}
	if opts.MaxRequests <= 0 {
		opts.MaxRequests = defaults.MaxRequests
	}
	if opts.MaxRetainedBytes <= 0 {
		opts.MaxRetainedBytes = defaults.MaxRetainedBytes
	}
	if opts.MaxAnnotations <= 0 {
		opts.MaxAnnotations = defaults.MaxAnnotations
	}
	if opts.MaxArtifacts <= 0 {
		opts.MaxArtifacts = defaults.MaxArtifacts
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = defaults.MaxLogBytes
	}
	if opts.MaxLogBytes > 16<<20 {
		opts.MaxLogBytes = 16 << 20
	}
	if opts.MaxExcerpts <= 0 {
		opts.MaxExcerpts = defaults.MaxExcerpts
	}
	if opts.ContextLines < 0 {
		opts.ContextLines = defaults.ContextLines
	}
	if opts.MaxExcerptLines <= 0 {
		opts.MaxExcerptLines = defaults.MaxExcerptLines
	}
	if opts.MaxLineCharacters <= 0 {
		opts.MaxLineCharacters = defaults.MaxLineCharacters
	}
	if opts.MaxLogLines <= 0 {
		opts.MaxLogLines = defaults.MaxLogLines
	}
	return opts
}

func (c *collector) pinAttempt(ref github.WorkflowRun) (github.WorkflowRun, bool) {
	if ref.Attempt != 0 {
		return ref, true
	}
	c.snapshot.Provenance.AttemptSelection = "latest_resolved_once"
	var raw restRun
	endpoint := runBaseEndpoint(ref) + "/" + strconv.FormatInt(ref.RunID, 10)
	if !c.rest(SourceRun, endpoint, &raw) {
		return ref, false
	}
	if raw.ID != ref.RunID || raw.RunAttempt <= 0 || !sameRepository(raw.Repository.FullName, ref) {
		c.fail(SourceRun, "invalid_response", "latest workflow run response omitted or changed identity fields")
		return ref, false
	}
	ref.Attempt = raw.RunAttempt
	return ref, true
}

func (c *collector) readRun(ref github.WorkflowRun, final bool) (WorkflowRunEvidence, bool) {
	var raw restRun
	if !c.rest(SourceRun, attemptEndpoint(ref), &raw) {
		return WorkflowRunEvidence{}, false
	}
	run := raw.normalize(c.opts.MaxLineCharacters)
	if run.ID != ref.RunID || run.Attempt != ref.Attempt || run.WorkflowID <= 0 || run.HeadSHA == "" ||
		run.Event == "" || run.Status == "" || run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() ||
		!sameRepository(raw.Repository.FullName, ref) {
		c.fail(SourceRun, "invalid_response", "pinned workflow run response omitted or changed identity fields")
		return WorkflowRunEvidence{}, false
	}
	c.incrementItems(SourceRun, 1)
	if final {
		c.end = identityOf(run)
		c.endOK = true
	} else if !c.retain(SourceRun, run) {
		return WorkflowRunEvidence{}, false
	}
	return run, true
}

func (c *collector) collectJobs(ref github.WorkflowRun) ([]Job, bool) {
	key := SourceJobs
	base := attemptEndpoint(ref) + "/jobs"
	jobs := make([]Job, 0)
	seen := make(map[int64]struct{})
	total := -1
	for page := 1; page <= c.opts.MaxPages; page++ {
		var response restJobsResponse
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &response) {
			return jobs, false
		}
		if response.TotalCount == nil || *response.TotalCount < 0 {
			c.fail(key, "invalid_response", "job response omitted total_count")
			return jobs, false
		}
		if total < 0 {
			total = *response.TotalCount
		} else if total != *response.TotalCount {
			c.fail(key, "inconsistent_response", "job total_count changed between pages")
			return jobs, false
		}
		for _, raw := range response.Jobs {
			job := raw.normalize(c.opts.MaxLineCharacters)
			if job.Attempt == 0 {
				job.Attempt = ref.Attempt
			}
			if job.ID <= 0 || job.RunID != ref.RunID || job.Attempt != ref.Attempt || job.Name == "" || job.Status == "" || job.HeadSHA == "" {
				c.fail(key, "invalid_response", "job response omitted or changed identity fields")
				return jobs, false
			}
			if _, duplicate := seen[job.ID]; duplicate {
				c.fail(key, "duplicate_item", "job response repeated an ID")
				return jobs, false
			}
			seen[job.ID] = struct{}{}
			if len(jobs) >= c.opts.MaxJobs {
				c.fail(key, "job_limit", "job limit reached before total_count")
				return jobs, false
			}
			jobs = append(jobs, job)
		}
		if len(jobs) == total {
			break
		}
		if len(jobs) > total {
			c.fail(key, "inconsistent_response", "job count exceeded total_count")
			return jobs, false
		}
		if len(response.Jobs) < c.opts.PerPage {
			c.fail(key, "incomplete_response", "job count did not match total_count")
			return jobs, false
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "job page limit reached")
			return jobs, false
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	if len(jobs) == 0 && diagnosticConclusion(c.snapshot.Run.Conclusion) {
		c.addLimitation(key, "The exact-attempt jobs endpoint returned no jobs for a diagnostic run conclusion; job diagnostics are unavailable.")
		c.fail(key, "no_jobs_for_diagnostic_run", "workflow run had a diagnostic conclusion but the exact-attempt endpoint returned no jobs")
		return jobs, false
	}
	for _, job := range jobs {
		if ref.JobID != 0 && job.ID != ref.JobID {
			continue
		}
		if !c.retain(key, job) {
			return jobs, false
		}
		c.incrementItems(key, 1)
	}
	if ref.JobID != 0 {
		c.addLimitation(key, "Only the selected job is returned; membership was checked against the complete exact-attempt job list.")
	}
	c.complete(key)
	return jobs, true
}

func selectJobs(jobs []Job, jobID int64) ([]Job, bool) {
	if jobID == 0 {
		return jobs, true
	}
	for _, job := range jobs {
		if job.ID == jobID {
			return []Job{job}, true
		}
	}
	return []Job{}, false
}

func (c *collector) collectAnnotations(ref github.WorkflowRun) {
	key := SourceAnnotations
	c.addLimitation(key, "Annotations are read from the current check-run resource and may not represent its historical state during the selected attempt.")
	c.addLimitation(key, "GitHub annotation objects have no stable ID; capture_ordinal and source page/index identify only this snapshot.")
	targets := c.annotationTargetIndexes()
	if len(targets) == 0 {
		if len(c.snapshot.Jobs) == 0 && diagnosticConclusion(c.snapshot.Run.Conclusion) {
			c.fail(key, "diagnostics_unavailable", "no jobs were available for annotation collection")
			return
		}
		c.captureFrom(key, SourceJobs)
		c.complete(key)
		return
	}
	for _, index := range targets {
		job := &c.snapshot.Jobs[index]
		checkRunID, ok := parseCheckRunURL(job.CheckRunURL, ref)
		if !ok {
			c.fail(key, "invalid_check_run_url", "job check_run_url did not match the selected API host and repository")
			continue
		}
		expectedAnnotations, ok := c.verifyCurrentCheckRun(ref, *job, checkRunID)
		if !ok {
			continue
		}
		if expectedAnnotations == 0 {
			continue
		}
		base := repoEndpoint(ref) + "/check-runs/" + strconv.FormatInt(checkRunID, 10) + "/annotations"
		collected := 0
		for page := 1; page <= c.opts.MaxPages; page++ {
			var response []restAnnotation
			if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &response) {
				break
			}
			for sourceIndex, raw := range response {
				if len(c.snapshot.Annotations) >= c.opts.MaxAnnotations {
					c.fail(key, "annotation_limit", "annotation limit reached")
					break
				}
				annotation := raw.normalize(len(c.snapshot.Annotations)+1, page, sourceIndex+1, job.ID, checkRunID, c.opts.MaxLineCharacters)
				if !c.retain(key, annotation) {
					break
				}
				c.snapshot.Annotations = append(c.snapshot.Annotations, annotation)
				c.incrementItems(key, 1)
				collected++
			}
			if collected == expectedAnnotations {
				break
			}
			if collected > expectedAnnotations {
				c.fail(key, "inconsistent_response", "annotation count exceeded current check-run metadata")
				break
			}
			if c.snapshot.Sources[key].Error != nil {
				break
			}
			if len(response) < c.opts.PerPage {
				c.fail(key, "incomplete_response", "annotation count did not match current check-run metadata")
				break
			}
			if page == c.opts.MaxPages {
				c.fail(key, "page_limit", "annotation page limit reached")
			}
		}
		if collected != expectedAnnotations && c.snapshot.Sources[key].Error == nil {
			c.fail(key, "incomplete_response", "annotation count did not match current check-run metadata")
		}
	}
	if c.snapshot.Sources[key].Error == nil {
		c.complete(key)
	}
}

func (c *collector) verifyCurrentCheckRun(ref github.WorkflowRun, job Job, checkRunID int64) (int, bool) {
	key := SourceAnnotations
	var check restCheckRun
	endpoint := repoEndpoint(ref) + "/check-runs/" + strconv.FormatInt(checkRunID, 10)
	if !c.rest(key, endpoint, &check) {
		return 0, false
	}
	if check.ID != checkRunID || check.HeadSHA != job.HeadSHA || check.Status == "" {
		c.fail(key, "check_run_identity_mismatch", "current check-run identity did not match the selected job")
		return 0, false
	}
	if job.Status != "" && check.Status != job.Status {
		c.fail(key, "check_run_state_changed", "current check-run status differed from the selected job")
		return 0, false
	}
	if job.Conclusion != "" && check.Conclusion != "" && check.Conclusion != job.Conclusion {
		c.fail(key, "check_run_state_changed", "current check-run conclusion differed from the selected job")
		return 0, false
	}
	if check.Output.AnnotationsCount == nil || *check.Output.AnnotationsCount < 0 {
		c.fail(key, "invalid_response", "current check-run metadata omitted annotations_count")
		return 0, false
	}
	return *check.Output.AnnotationsCount, true
}

func (c *collector) collectLogs(ref github.WorkflowRun) {
	key := SourceLogs
	targets := c.logTargetIndexes()
	if len(targets) == 0 {
		for index := range c.snapshot.Jobs {
			reason := "conclusion_not_diagnostic"
			if c.snapshot.Selection.Scope == "job" && noExecution(c.snapshot.Jobs[index]) {
				reason = "no_execution"
			}
			c.snapshot.Jobs[index].Log = skippedLog(reason)
		}
		if len(c.snapshot.Jobs) == 0 && diagnosticConclusion(c.snapshot.Run.Conclusion) {
			c.fail(key, "diagnostics_unavailable", "no jobs were available for log collection")
			return
		}
		c.captureFrom(key, SourceJobs)
		c.complete(key)
		return
	}
	annotationByJob := make(map[int64][]Annotation)
	for _, annotation := range c.snapshot.Annotations {
		annotationByJob[annotation.JobID] = append(annotationByJob[annotation.JobID], annotation)
	}
	for index := range c.snapshot.Jobs {
		if !containsIndex(targets, index) {
			reason := "conclusion_not_diagnostic"
			if c.snapshot.Selection.Scope == "job" && noExecution(c.snapshot.Jobs[index]) {
				reason = "no_execution"
			}
			c.snapshot.Jobs[index].Log = skippedLog(reason)
		}
	}
	processed := 0
	for _, index := range targets {
		job := &c.snapshot.Jobs[index]
		if processed >= c.opts.MaxLogJobs {
			job.Log = skippedLog("log_job_limit")
			c.fail(key, "log_job_limit", "log job limit reached")
			continue
		}
		processed++
		endpoint := repoEndpoint(ref) + "/actions/jobs/" + strconv.FormatInt(job.ID, 10) + "/logs"
		data, err := c.raw(key, endpoint)
		log := LogEvidence{}
		if err == nil {
			if len(data) > c.opts.MaxLogBytes {
				data = data[:c.opts.MaxLogBytes]
				log = processLog(data, job.FailedStepNames, annotationByJob[job.ID], c.opts, true)
				log.State = "truncated"
				log.Reason = "log_byte_limit"
				log.RetrievalComplete = false
				log.Error = &evidence.Error{Code: "response_limit", Message: "job log exceeded the configured bounded prefix"}
				c.fail(key, "response_limit", "one or more job logs exceeded the configured bounded prefix")
			} else {
				log = processLog(data, job.FailedStepNames, annotationByJob[job.ID], c.opts, false)
			}
		} else {
			var apiError *github.Error
			if errors.As(err, &apiError) && apiError.Kind == github.KindResponseLimit && len(data) > 0 {
				if len(data) > c.opts.MaxLogBytes {
					data = data[:c.opts.MaxLogBytes]
				}
				log = processLog(data, job.FailedStepNames, annotationByJob[job.ID], c.opts, true)
				log.State = "truncated"
				log.Reason = "response_limit"
				log.RetrievalComplete = false
				log.Error = &evidence.Error{Code: "response_limit", Message: "job log exceeded the configured bounded prefix"}
			} else {
				log = unavailableLog(err)
			}
			c.fail(key, log.Error.Code, log.Error.Message)
		}
		if log.OmittedExcerpts > 0 {
			c.fail(key, "excerpt_limit", "log excerpt selection omitted additional matching regions")
		}
		if log.OmittedLines > 0 {
			c.fail(key, "line_limit", "log line processing limit reached")
		}
		if log.TruncatedLines > 0 {
			c.fail(key, "line_character_limit", "one or more log lines exceeded the character limit before marker selection")
		}
		if !c.retain(key, log) {
			job.Log = skippedLog("retention_limit")
			continue
		}
		job.Log = log
		c.incrementItems(key, 1)
	}
	if c.snapshot.Sources[key].Error == nil {
		c.complete(key)
	}
}

func unavailableLog(err error) LogEvidence {
	state := "unavailable"
	reason := "request_failed"
	code := "request_failed"
	message := "job log could not be retrieved"
	var apiError *github.Error
	if errors.As(err, &apiError) {
		switch apiError.Kind {
		case github.KindNotFound:
			reason, code, message = "expired_or_unavailable", "not_found", "job log was unavailable; GitHub does not distinguish an expired log from an unknown job"
		case github.KindGone:
			reason, code, message = "gone", "gone", "job log resource was no longer available"
		case github.KindPermissionDenied:
			state, reason, code, message = "permission_denied", "permission_denied", "permission_denied", "GitHub denied access to the job log"
		case github.KindResponseLimit:
			state, reason, code, message = "truncated", "response_limit", "response_limit", "job log exceeded the configured response limit"
		case github.KindTimeout:
			reason, code, message = "timeout", "timeout", "job log request timed out or was canceled"
		}
	}
	return LogEvidence{
		State:                    state,
		Reason:                   reason,
		RetrievalComplete:        false,
		ExcerptSelectionComplete: false,
		Excerpts:                 []LogExcerpt{},
		Error:                    &evidence.Error{Code: code, Message: message},
	}
}

func skippedLog(reason string) LogEvidence {
	return LogEvidence{State: "skipped", Reason: reason, RetrievalComplete: true, ExcerptSelectionComplete: true, Excerpts: []LogExcerpt{}}
}

func (c *collector) collectArtifacts(ref github.WorkflowRun) {
	key := SourceArtifacts
	c.addLimitation(key, c.snapshot.Artifacts.Limitation)
	base := runBaseEndpoint(ref) + "/" + strconv.FormatInt(ref.RunID, 10) + "/artifacts"
	seen := make(map[int64]struct{})
	total := -1
	for page := 1; page <= c.opts.MaxPages; page++ {
		var response restArtifactsResponse
		if !c.rest(key, pagedEndpoint(base, c.opts.PerPage, page), &response) {
			return
		}
		if response.TotalCount == nil || *response.TotalCount < 0 {
			c.fail(key, "invalid_response", "artifact response omitted total_count")
			return
		}
		if total < 0 {
			total = *response.TotalCount
		} else if total != *response.TotalCount {
			c.fail(key, "inconsistent_response", "artifact total_count changed between pages")
			return
		}
		for _, raw := range response.Artifacts {
			if raw.ID <= 0 || raw.Name == "" {
				c.fail(key, "invalid_response", "artifact response omitted stable identity")
				return
			}
			if _, duplicate := seen[raw.ID]; duplicate {
				c.fail(key, "duplicate_item", "artifact response repeated an ID")
				return
			}
			seen[raw.ID] = struct{}{}
			if len(c.snapshot.Artifacts.Items) >= c.opts.MaxArtifacts {
				c.fail(key, "artifact_limit", "artifact limit reached before total_count")
				return
			}
			artifact := raw.normalize(c.opts.MaxLineCharacters)
			if !c.retain(key, artifact) {
				return
			}
			c.snapshot.Artifacts.Items = append(c.snapshot.Artifacts.Items, artifact)
			c.incrementItems(key, 1)
		}
		if len(c.snapshot.Artifacts.Items) == total {
			c.complete(key)
			return
		}
		if len(c.snapshot.Artifacts.Items) > total {
			c.fail(key, "inconsistent_response", "artifact count exceeded total_count")
			return
		}
		if len(response.Artifacts) < c.opts.PerPage {
			c.fail(key, "incomplete_response", "artifact count did not match total_count")
			return
		}
		if page == c.opts.MaxPages {
			c.fail(key, "page_limit", "artifact page limit reached")
		}
	}
}

func (c *collector) finishFreshness(ref github.WorkflowRun) {
	key := SourceFreshness
	c.snapshot.Freshness.Start = c.start
	if c.endOK {
		c.snapshot.Freshness.End = c.end
	}
	c.captureFrom(key, SourceRun)
	if c.start.ID == 0 || !c.endOK {
		c.fail(key, "identity_unavailable", "start and end pinned-attempt identity reads did not both succeed")
		return
	}
	runSource := c.snapshot.Sources[SourceRun]
	if runSource.Error == nil && runSource.Items == 2 {
		runSource.Complete = true
		c.snapshot.Sources[SourceRun] = runSource
	}
	c.snapshot.Freshness.ChangedFields = changedRunFields(c.start, c.end)
	c.snapshot.Freshness.Ongoing = !terminalStatus(c.end.Status)
	if len(c.snapshot.Freshness.ChangedFields) > 0 {
		c.fail(key, "snapshot_changed", "workflow run identity changed during collection")
		return
	}
	if c.snapshot.Freshness.Ongoing {
		c.fail(key, "run_in_progress", "workflow run was not complete at the end of collection")
		return
	}
	c.complete(key)
}

func changedRunFields(start, end RunIdentity) []string {
	var changed []string
	if start.ID != end.ID {
		changed = append(changed, "id")
	}
	if start.Attempt != end.Attempt {
		changed = append(changed, "attempt")
	}
	if start.WorkflowID != end.WorkflowID {
		changed = append(changed, "workflow_id")
	}
	if start.HeadSHA != end.HeadSHA {
		changed = append(changed, "head_sha")
	}
	if start.Event != end.Event {
		changed = append(changed, "event")
	}
	if start.Status != end.Status {
		changed = append(changed, "status")
	}
	if start.Conclusion != end.Conclusion {
		changed = append(changed, "conclusion")
	}
	if !start.CreatedAt.Equal(end.CreatedAt) {
		changed = append(changed, "created_at")
	}
	if !start.UpdatedAt.Equal(end.UpdatedAt) {
		changed = append(changed, "updated_at")
	}
	if !sameTimePointer(start.RunStartedAt, end.RunStartedAt) {
		changed = append(changed, "run_started_at")
	}
	return changed
}

func sameTimePointer(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func identityOf(run WorkflowRunEvidence) RunIdentity {
	return RunIdentity{ID: run.ID, Attempt: run.Attempt, WorkflowID: run.WorkflowID, HeadSHA: run.HeadSHA,
		Event: run.Event, Status: run.Status, Conclusion: run.Conclusion, CreatedAt: run.CreatedAt,
		UpdatedAt: run.UpdatedAt, RunStartedAt: run.RunStartedAt}
}

func diagnosticJobIndexes(jobs []Job) []int {
	var indexes []int
	for index := range jobs {
		if diagnosticConclusion(jobs[index].Conclusion) {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func (c *collector) annotationTargetIndexes() []int {
	indexes := make([]int, len(c.snapshot.Jobs))
	for index := range indexes {
		indexes[index] = index
	}
	return indexes
}

func (c *collector) logTargetIndexes() []int {
	if c.snapshot.Selection.Scope == "job" {
		var indexes []int
		for index := range c.snapshot.Jobs {
			if !noExecution(c.snapshot.Jobs[index]) {
				indexes = append(indexes, index)
			}
		}
		return indexes
	}
	return diagnosticJobIndexes(c.snapshot.Jobs)
}

func noExecution(job Job) bool {
	return strings.EqualFold(job.Conclusion, "skipped") && job.StartedAt == nil && len(job.Steps) == 0
}

func containsIndex(indexes []int, target int) bool {
	for _, index := range indexes {
		if index == target {
			return true
		}
	}
	return false
}

func diagnosticConclusion(conclusion string) bool {
	switch strings.ToLower(conclusion) {
	case "failure", "cancelled", "canceled", "timed_out", "action_required", "startup_failure", "stale":
		return true
	default:
		return false
	}
}

func terminalStatus(status string) bool {
	switch strings.ToLower(status) {
	case "completed":
		return true
	default:
		return false
	}
}

func (c *collector) rest(key, endpoint string, out any) bool {
	if !c.reserveRequest(key) {
		return false
	}
	err := c.api.REST(c.ctx, endpoint, out)
	c.captureNow(key)
	if err != nil {
		code, message := classifyAPIError(err)
		c.fail(key, code, message)
		return false
	}
	return true
}

func (c *collector) raw(key, endpoint string) ([]byte, error) {
	if !c.reserveRequest(key) {
		return nil, errors.New("request limit reached")
	}
	data, err := c.api.RawREST(c.ctx, endpoint)
	c.captureNow(key)
	return data, err
}

func (c *collector) reserveRequest(key string) bool {
	if c.requests >= c.opts.MaxRequests {
		c.fail(key, "request_limit", "global API request limit reached")
		return false
	}
	c.requests++
	source := c.snapshot.Sources[key]
	source.Requests++
	c.snapshot.Sources[key] = source
	return true
}

func classifyAPIError(err error) (string, string) {
	var apiError *github.Error
	if errors.As(err, &apiError) {
		return string(apiError.Kind), apiError.Message
	}
	return "request_failed", "GitHub request failed"
}

func (c *collector) retain(key string, value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		c.fail(key, "normalization_failed", "evidence could not be normalized")
		return false
	}
	if len(encoded) > c.opts.MaxRetainedBytes-c.retainedBytes {
		c.fail(key, "retention_limit", "global retained evidence limit reached")
		return false
	}
	c.retainedBytes += len(encoded)
	return true
}

func (c *collector) failDependents(message string) {
	for _, key := range []string{SourceJobs, SourceLogs, SourceAnnotations, SourceArtifacts, SourceFreshness} {
		c.fail(key, "dependency_failed", message)
	}
}

func (c *collector) fail(key, code, message string) {
	source := c.snapshot.Sources[key]
	source.Complete = false
	if source.Error == nil {
		source.Error = &evidence.Error{Code: code, Message: message}
	}
	c.snapshot.Sources[key] = source
}

func (c *collector) complete(key string) {
	source := c.snapshot.Sources[key]
	if source.Error == nil {
		source.Complete = true
	}
	c.snapshot.Sources[key] = source
}

func (c *collector) incrementItems(key string, count int) {
	source := c.snapshot.Sources[key]
	source.Items += count
	c.snapshot.Sources[key] = source
}

func (c *collector) addLimitation(key, limitation string) {
	source := c.snapshot.Sources[key]
	for _, existing := range source.Limitations {
		if existing == limitation {
			return
		}
	}
	source.Limitations = append(source.Limitations, limitation)
	c.snapshot.Sources[key] = source
}

func (c *collector) captureNow(key string) {
	source := c.snapshot.Sources[key]
	source.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.snapshot.Sources[key] = source
}

func (c *collector) captureFrom(key, sourceKey string) {
	source := c.snapshot.Sources[key]
	if source.CapturedAt == "" {
		source.CapturedAt = c.snapshot.Sources[sourceKey].CapturedAt
	}
	c.snapshot.Sources[key] = source
}

func (c *collector) finish() {
	finished := time.Now().UTC()
	c.snapshot.FinishedAt = finished
	complete := true
	for _, key := range sourceKeys {
		source := c.snapshot.Sources[key]
		if source.CapturedAt == "" {
			source.CapturedAt = finished.Format(time.RFC3339Nano)
			c.snapshot.Sources[key] = source
		}
		if !source.Complete {
			complete = false
		}
	}
	c.snapshot.Complete = complete
}

func runBaseEndpoint(ref github.WorkflowRun) string {
	return repoEndpoint(ref) + "/actions/runs"
}

func attemptEndpoint(ref github.WorkflowRun) string {
	return runBaseEndpoint(ref) + "/" + strconv.FormatInt(ref.RunID, 10) + "/attempts/" + strconv.Itoa(ref.Attempt)
}

func repoEndpoint(ref github.WorkflowRun) string {
	return "repos/" + ref.Owner + "/" + ref.Name
}

func pagedEndpoint(base string, perPage, page int) string {
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + "per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
}

func sameRepository(fullName string, ref github.WorkflowRun) bool {
	return strings.EqualFold(fullName, ref.Owner+"/"+ref.Name)
}

func parseCheckRunURL(raw string, ref github.WorkflowRun) (int64, bool) {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Port() != "" {
		return 0, false
	}
	expectedHost := ref.Host
	prefix := "/api/v3/repos/"
	if strings.EqualFold(ref.Host, "github.com") {
		expectedHost = "api.github.com"
		prefix = "/repos/"
	}
	if !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, prefix), "/")
	if !strings.HasPrefix(parsed.Path, prefix) || len(parts) != 4 || !strings.EqualFold(parts[0], ref.Owner) || !strings.EqualFold(parts[1], ref.Name) || parts[2] != "check-runs" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[3], 10, 64)
	return id, err == nil && id > 0
}

func sanitizeText(value string, maxCharacters int) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	lines = redactPrivateKeyBlocks(lines)
	for index := range lines {
		lines[index] = sanitizeLogLine(lines[index], maxCharacters)
	}
	return truncateRunes(strings.Join(lines, "\n"), maxCharacters)
}

type restRun struct {
	ID           int64      `json:"id"`
	RunAttempt   int        `json:"run_attempt"`
	WorkflowID   int64      `json:"workflow_id"`
	Name         string     `json:"name"`
	DisplayTitle string     `json:"display_title"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HeadSHA      string     `json:"head_sha"`
	HeadBranch   string     `json:"head_branch"`
	URL          string     `json:"url"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
	Repository   struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (raw restRun) normalize(textLimit int) WorkflowRunEvidence {
	return WorkflowRunEvidence{ID: raw.ID, Attempt: raw.RunAttempt, WorkflowID: raw.WorkflowID,
		Name: sanitizeText(raw.Name, textLimit), DisplayTitle: sanitizeText(raw.DisplayTitle, textLimit),
		Event: raw.Event, Status: raw.Status, Conclusion: raw.Conclusion, HeadSHA: raw.HeadSHA,
		HeadBranch: sanitizeText(raw.HeadBranch, textLimit), URL: raw.URL, HTMLURL: raw.HTMLURL,
		CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt, RunStartedAt: raw.RunStartedAt}
}

type restJobsResponse struct {
	TotalCount *int      `json:"total_count"`
	Jobs       []restJob `json:"jobs"`
}

type restJob struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	RunAttempt  int        `json:"run_attempt"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	HTMLURL     string     `json:"html_url"`
	CheckRunURL string     `json:"check_run_url"`
	RunnerName  string     `json:"runner_name"`
	Labels      []string   `json:"labels"`
	Steps       []restStep `json:"steps"`
}

func (raw restJob) normalize(textLimit int) Job {
	job := Job{ID: raw.ID, RunID: raw.RunID, Attempt: raw.RunAttempt, Name: sanitizeText(raw.Name, textLimit),
		HeadSHA: raw.HeadSHA, Status: raw.Status, Conclusion: raw.Conclusion, StartedAt: raw.StartedAt,
		CompletedAt: raw.CompletedAt, HTMLURL: raw.HTMLURL, CheckRunURL: raw.CheckRunURL,
		RunnerName: sanitizeText(raw.RunnerName, textLimit), Labels: []string{}, Steps: []Step{},
		FailedStepNames: []string{}, Log: skippedLog("not_collected")}
	for _, label := range raw.Labels {
		job.Labels = append(job.Labels, sanitizeText(label, textLimit))
	}
	for _, rawStep := range raw.Steps {
		step := rawStep.normalize(textLimit)
		job.Steps = append(job.Steps, step)
		if diagnosticConclusion(step.Conclusion) {
			job.FailedStepNames = append(job.FailedStepNames, step.Name)
		}
	}
	return job
}

type restStep struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func (raw restStep) normalize(textLimit int) Step {
	return Step{Number: raw.Number, Name: sanitizeText(raw.Name, textLimit), Status: raw.Status,
		Conclusion: raw.Conclusion, StartedAt: raw.StartedAt, CompletedAt: raw.CompletedAt}
}

type restCheckRun struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		AnnotationsCount *int `json:"annotations_count"`
	} `json:"output"`
}

type restAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	StartColumn     int    `json:"start_column"`
	EndColumn       int    `json:"end_column"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title"`
	RawDetails      string `json:"raw_details"`
	BlobURL         string `json:"blob_href"`
}

func (raw restAnnotation) normalize(ordinal, page, sourceIndex int, jobID, checkRunID int64, textLimit int) Annotation {
	return Annotation{CaptureOrdinal: ordinal, SourcePage: page, SourceIndex: sourceIndex,
		JobID: jobID, CheckRunID: checkRunID, Path: sanitizeText(raw.Path, textLimit),
		StartLine: raw.StartLine, EndLine: raw.EndLine, StartColumn: raw.StartColumn, EndColumn: raw.EndColumn,
		AnnotationLevel: raw.AnnotationLevel,
		Message:         sanitizeText(raw.Message, textLimit), Title: sanitizeText(raw.Title, textLimit),
		RawDetails: sanitizeText(raw.RawDetails, textLimit), BlobURL: sanitizeText(raw.BlobURL, textLimit),
		Scope:                 "current_check_run_resource",
		HistoricalAttribution: "unverified"}
}

type restArtifactsResponse struct {
	TotalCount *int           `json:"total_count"`
	Artifacts  []restArtifact `json:"artifacts"`
}

type restArtifact struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	SizeBytes int64      `json:"size_in_bytes"`
	Expired   bool       `json:"expired"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	URL       string     `json:"url"`
	Digest    string     `json:"digest"`
}

func (raw restArtifact) normalize(textLimit int) Artifact {
	return Artifact{ID: raw.ID, Name: sanitizeText(raw.Name, textLimit), SizeBytes: raw.SizeBytes,
		Expired: raw.Expired, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt, ExpiresAt: raw.ExpiresAt,
		URL: raw.URL, Digest: sanitizeText(raw.Digest, textLimit)}
}

func formatReference(ref github.WorkflowRun) string {
	return fmt.Sprintf("%s/%s run %d attempt %d", ref.Owner, ref.Name, ref.RunID, ref.Attempt)
}

package prdelta

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"aid/internal/github"
)

// GitHub's compare endpoint returns at most this many files for a comparison.
const compareFileLimit = 300

type restCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

type restFile struct {
	Filename         string  `json:"filename"`
	Status           string  `json:"status"`
	PreviousFilename string  `json:"previous_filename"`
	Patch            *string `json:"patch"`
	SHA              string  `json:"sha"`
	Changes          int     `json:"changes"`
}

type restCompare struct {
	Status          string `json:"status"`
	AheadBy         int    `json:"ahead_by"`
	TotalCommits    *int   `json:"total_commits"`
	MergeBaseCommit *struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Commits []restCommit `json:"commits"`
	Files   []restFile   `json:"files"`
}

type restCommitDetail struct {
	restCommit
	Files []restFile `json:"files"`
}

// compareSide is a fetched three-dot compare with all of its commits when
// commitsComplete is true.
type compareSide struct {
	base            string
	head            string
	response        restCompare
	commitsComplete bool
}

func (side compareSide) describe() DiffSide {
	result := DiffSide{Commit: side.head, Base: side.base, Status: side.response.Status, Files: len(side.response.Files)}
	if side.response.MergeBaseCommit != nil {
		result.MergeBase = side.response.MergeBaseCommit.SHA
	}
	if side.response.TotalCommits != nil {
		result.Commits = *side.response.TotalCommits
	}
	return result
}

func (side compareSide) mergeBase() string {
	if side.response.MergeBaseCommit == nil {
		return ""
	}
	return side.response.MergeBaseCommit.SHA
}

// compare reads base...head. With wantCommits it pages until every commit in
// total_commits is present or the page bound is reached.
func (c *collector) compare(key, base, head string, wantCommits bool) (compareSide, bool) {
	side := compareSide{base: base, head: head}
	if !c.rest(key, side.endpoint(c)+pageQuery(1), &side.response, false) {
		return side, false
	}
	if side.response.Status == "" || side.response.TotalCommits == nil {
		c.fail(key, "invalid_response", "compare response omitted status or commit count")
		return side, false
	}
	side.commitsComplete = len(side.response.Commits) == *side.response.TotalCommits
	if wantCommits {
		return side, c.moreCommits(key, &side)
	}
	return side, true
}

func (side compareSide) endpoint(c *collector) string {
	return c.repoEndpoint() + "/compare/" + side.base + "..." + side.head + "?"
}

// moreCommits pages a compare's commit list. Reaching the page bound leaves
// commitsComplete false without failing the request.
func (c *collector) moreCommits(key string, side *compareSide) bool {
	total := *side.response.TotalCommits
	for page := 2; len(side.response.Commits) < total && page <= c.opts.MaxPages; page++ {
		var next restCompare
		if !c.rest(key, side.endpoint(c)+pageQuery(page), &next, false) {
			return false
		}
		if len(next.Commits) == 0 {
			break
		}
		side.response.Commits = append(side.response.Commits, next.Commits...)
	}
	side.commitsComplete = len(side.response.Commits) == total
	return true
}

// collectDelta computes the head relation, PR diff comparison, and (for
// rebases) patch-equivalent commit matching.
func (c *collector) collectDelta(pull restPull, baseline string) {
	head := pull.Head.SHA
	c.collectHeadRelation(baseline, head)
	headSide, baselineSide, ok := c.collectPRDiff(pull, baseline, head)
	if c.result.HeadRelation == RelationRebased {
		c.collectEquivalence(headSide, baselineSide, ok)
	}
}

func (c *collector) collectHeadRelation(baseline, head string) {
	key := SourceHeadRelation
	if baseline == head {
		c.result.HeadRelation = RelationIdentical
		c.result.NewCommits = []CommitRef{}
		c.complete(key)
		return
	}
	side, ok := c.compare(key, baseline, head, false)
	if ok && side.response.Status == "ahead" {
		ok = c.moreCommits(key, &side)
	}
	if !ok {
		c.result.NewCommitsNote = "baseline...head compare was unavailable"
		return
	}
	switch side.response.Status {
	case "identical":
		c.result.HeadRelation = RelationIdentical
		c.result.NewCommits = []CommitRef{}
	case "ahead":
		c.result.HeadRelation = RelationFastForward
		c.result.NewCommits = commitRefs(side.response.Commits)
		if !side.commitsComplete {
			c.fail(key, "page_limit", "compare commit page limit reached; new_commits is partial")
			c.result.NewCommitsNote = "partial: compare commit page limit reached"
		}
	case "behind":
		c.result.HeadRelation = RelationBehind
		c.result.NewCommits = []CommitRef{}
		c.result.NewCommitsNote = "head is an ancestor of the baseline"
	case "diverged":
		c.result.HeadRelation = RelationRebased
	default:
		c.fail(key, "invalid_response", "compare returned an unknown status")
		return
	}
	c.addItems(key, len(side.response.Commits))
	c.complete(key)
}

func commitRefs(commits []restCommit) []CommitRef {
	refs := make([]CommitRef, 0, len(commits))
	for _, commit := range commits {
		subject, _, _ := strings.Cut(commit.Commit.Message, "\n")
		refs = append(refs, CommitRef{SHA: commit.SHA, MessageFirstLine: strings.TrimSpace(subject)})
	}
	return refs
}

// diffBase selects the compare base and resolves it to a commit once. Open pull
// requests use the current base branch, or the default branch when that branch
// was deleted. Closed and merged pull requests use the base SHA GitHub recorded
// when they closed; their head is usually contained in the base branch, which
// would empty a branch-based diff.
func (c *collector) diffBase(pull restPull) (DiffBase, bool) {
	info := c.result.PR
	if info.State != "open" && isSHA(info.BaseSHA) {
		recorded := strings.ToLower(info.BaseSHA)
		return DiffBase{Ref: info.BaseSHA, SHA: recorded, Source: "recorded_base_sha"}, true
	}
	base := DiffBase{Ref: pull.Base.Ref, Source: "base_ref"}
	if exists := c.result.Stack.BaseBranchExists; exists != nil && !*exists && info.DefaultBranch != "" {
		base = DiffBase{Ref: info.DefaultBranch, Source: "default_branch"}
	}
	tip, ok := c.branchTip(SourcePRDiff, base.Ref, false)
	if !ok {
		return base, false
	}
	base.SHA = tip
	c.baseBranch, c.baseSHA = base.Ref, tip
	c.reserved++
	return base, true
}

const branchTipQuery = `query($owner: String!, $name: String!, $qualified: String!) {
  repository(owner: $owner, name: $name) {
    ref(qualifiedName: $qualified) { target { oid } }
  }
}`

// branchTip resolves a branch to its current commit. GraphQL takes the branch
// name as a variable, so any valid branch name works without path escaping.
func (c *collector) branchTip(key, branch string, final bool) (string, bool) {
	var data struct {
		Repository *struct {
			Ref *struct {
				Target *struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"repository"`
	}
	variables := map[string]any{"owner": c.ref.Owner, "name": c.ref.Name, "qualified": "refs/heads/" + branch}
	if !c.graphQL(key, branchTipQuery, variables, &data, final) {
		return "", false
	}
	switch {
	case data.Repository == nil:
		c.fail(key, "invalid_response", "branch lookup omitted the repository")
	case data.Repository.Ref == nil:
		c.fail(key, string(github.KindNotFound), "compare base branch was not found")
	case data.Repository.Ref.Target == nil || !isSHA(data.Repository.Ref.Target.OID):
		c.fail(key, "invalid_response", "branch lookup omitted the target commit")
	default:
		return strings.ToLower(data.Repository.Ref.Target.OID), true
	}
	return "", false
}

// checkBaseStable resolves the base branch again at the end of collection. A
// moved base means the compares may not describe the current pull request, so
// the PR diff source is marked incomplete with warning base_moved.
func (c *collector) checkBaseStable() {
	if c.baseBranch == "" {
		return
	}
	tip, ok := c.branchTip(SourcePRDiff, c.baseBranch, true)
	if !ok {
		return
	}
	if tip != c.baseSHA {
		c.warn("base_moved")
		c.fail(SourcePRDiff, "snapshot_changed", "compare base branch moved during collection")
	}
}

func (c *collector) collectPRDiff(pull restPull, baseline, head string) (compareSide, compareSide, bool) {
	key := SourcePRDiff
	base, ok := c.diffBase(pull)
	diff := &PRDiff{Verdict: VerdictUnknown, Base: base, Counts: map[string]int{}, Files: []FileDelta{}}
	c.result.PRDiff = diff
	if !ok {
		diff.Reason = "compare base could not be resolved to a commit"
		return compareSide{}, compareSide{}, false
	}
	headSide, ok := c.compare(key, base.SHA, head, true)
	if !ok {
		diff.Reason = "head-side compare was unavailable"
		return headSide, compareSide{}, false
	}
	diff.HeadSide = headSide.describe()
	baselineSide := headSide
	if baseline != head {
		baselineSide, ok = c.compare(key, base.SHA, baseline, true)
		if !ok {
			diff.Reason = "baseline-side compare was unavailable"
			return headSide, baselineSide, false
		}
	}
	diff.BaselineSide = baselineSide.describe()
	for _, check := range []struct {
		side compareSide
		name string
	}{{headSide, "head"}, {baselineSide, "baseline"}} {
		if check.side.response.Status == "identical" || check.side.response.Status == "behind" {
			c.warn(check.name + "_contained_in_base")
			diff.Reason = check.name + " commit is already contained in the compare base; its PR diff is empty"
			c.complete(key)
			return headSide, baselineSide, false
		}
	}
	if baseline != head && baselineSide.mergeBase() != headSide.mergeBase() {
		realigned, reason := c.realignBaseline(base.SHA, baselineSide)
		switch {
		case reason != "":
			diff.Reason = reason
			c.complete(key)
			return headSide, baselineSide, false
		case realigned != nil:
			baselineSide = *realigned
			diff.BaselineSide = baselineSide.describe()
		}
	}
	for _, side := range []compareSide{headSide, baselineSide} {
		if len(side.response.Files) >= compareFileLimit {
			c.fail(key, "api_limit", "compare returned GitHub's 300-file maximum; the file list may be truncated")
			diff.Reason = "compare file list may be truncated at 300 files"
			return headSide, baselineSide, false
		}
	}
	diff.Files = compareFiles(baselineSide.response.Files, headSide.response.Files, c.opts.MaxHunksPerFile)
	c.checkModes(diff.Files, baselineSide, headSide)
	sortFiles(diff.Files)
	diff.Verdict = verdict(diff.Files)
	for _, file := range diff.Files {
		diff.Counts[file.Status]++
	}
	c.addItems(key, len(diff.Files))
	if diff.Verdict == VerdictUnknown {
		diff.Reason = "at least one file patch was omitted by GitHub (binary or too large) or could not be parsed, or a file mode could not be read"
		c.fail(key, "patch_unavailable", diff.Reason)
	}
	c.complete(key)
	return headSide, baselineSide, true
}

// realignBaseline handles a base branch that was rewritten after the baseline
// review (for example, a rebased stack). The baseline-side compare then also
// contains the old base commits. Commits whose author identity, author date,
// and subject reappear in the current base history form a prefix that is
// excluded by comparing from the last such commit instead. It returns nil and
// no reason when no realignment is needed, and a reason when the diff is
// unknowable.
func (c *collector) realignBaseline(base string, side compareSide) (*compareSide, string) {
	key := SourcePRDiff
	if !side.commitsComplete {
		c.warn("baseline_base_unverified")
		c.addLimitation(key, "baseline-side commits exceed the page bound; base rewrite was not checked")
		return nil, ""
	}
	history, found := c.baseHistory(base, side.mergeBase())
	if !found {
		c.warn("baseline_base_unverified")
		c.addLimitation(key, "base history since the baseline merge base exceeds the page bound; base rewrite was not checked")
		return nil, ""
	}
	prefix := 0
	for prefix < len(side.response.Commits) && history[commitKey(side.response.Commits[prefix])] {
		prefix++
	}
	for _, commit := range side.response.Commits[prefix:] {
		if history[commitKey(commit)] {
			c.warn("baseline_base_ambiguous")
			return nil, "baseline commits interleave with commits now in the base; the baseline PR diff is ambiguous"
		}
	}
	if prefix == 0 {
		return nil, ""
	}
	if prefix == len(side.response.Commits) {
		c.warn("baseline_contained_in_base")
		return nil, "every baseline commit now appears in the base; the baseline PR diff is empty"
	}
	boundary := side.response.Commits[prefix-1].SHA
	next := side.response.Commits[prefix]
	if len(next.Parents) != 1 || next.Parents[0].SHA != boundary {
		c.warn("baseline_base_ambiguous")
		return nil, "rewritten base commits do not end at a single parent of the baseline commits"
	}
	realigned, ok := c.compare(key, boundary, side.head, true)
	if !ok {
		return nil, "realigned baseline-side compare was unavailable"
	}
	c.warn("baseline_base_realigned")
	c.addLimitation(key, strconv.Itoa(prefix)+" rewritten base commits were excluded from the baseline side")
	return &realigned, ""
}

// baseHistory walks the base commit's history until mergeBase and returns the
// identity keys of commits seen before it. base is the resolved base SHA, so
// the walk reads the same snapshot as the compares.
func (c *collector) baseHistory(base, mergeBase string) (map[string]bool, bool) {
	keys := map[string]bool{}
	for page := 1; page <= c.opts.MaxPages; page++ {
		query := url.Values{"sha": {base}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		var commits []restCommit
		if !c.rest(SourcePRDiff, c.repoEndpoint()+"/commits?"+query.Encode(), &commits, false) {
			return nil, false
		}
		for _, commit := range commits {
			if commit.SHA == mergeBase {
				return keys, true
			}
			keys[commitKey(commit)] = true
		}
		if len(commits) < perPage {
			return nil, false
		}
	}
	return nil, false
}

func commitKey(commit restCommit) string {
	subject, _, _ := strings.Cut(commit.Commit.Message, "\n")
	return strings.ToLower(commit.Commit.Author.Email) + "\x00" + commit.Commit.Author.Date + "\x00" + strings.TrimSpace(subject)
}

// collectEquivalence lists the PR's current commits whose patch has no
// equivalent among the baseline-side commits. Patch identity ignores hunk
// header offsets, like git patch-id ignores line numbers.
func (c *collector) collectEquivalence(headSide, baselineSide compareSide, diffOK bool) {
	key := SourceCommits
	omit := func(code, note string) {
		c.result.NewCommits = nil
		c.result.NewCommitsNote = note
		c.fail(key, code, note)
	}
	if !diffOK {
		omit("dependency_failed", "PR commits could not be compared because the PR diff was unavailable")
		return
	}
	if !headSide.commitsComplete || !baselineSide.commitsComplete {
		omit("page_limit", "PR commit lists exceed the page bound")
		return
	}
	baselineSHAs := map[string]bool{}
	for _, commit := range baselineSide.response.Commits {
		baselineSHAs[commit.SHA] = true
	}
	var pending []restCommit
	for _, commit := range headSide.response.Commits {
		if len(commit.Parents) > 1 {
			omit("merge_commit", "the PR contains merge commits; patch equivalence is not derived")
			return
		}
		if !baselineSHAs[commit.SHA] {
			pending = append(pending, commit)
		}
	}
	if len(pending) == 0 {
		c.result.NewCommits = []CommitRef{}
		c.complete(key)
		return
	}
	var candidates []restCommit
	headSHAs := map[string]bool{}
	for _, commit := range headSide.response.Commits {
		headSHAs[commit.SHA] = true
	}
	for _, commit := range baselineSide.response.Commits {
		if !headSHAs[commit.SHA] {
			candidates = append(candidates, commit)
		}
	}
	if len(pending)+len(candidates) > c.opts.MaxCommitPatches {
		omit("patch_limit", "commit patches needed exceed --max-commit-patches")
		return
	}
	identities := map[string]int{}
	for _, commit := range candidates {
		identity, ok := c.patchIdentity(commit.SHA)
		if !ok {
			omit("patch_unavailable", "a baseline commit patch was unavailable or truncated")
			return
		}
		identities[identity]++
	}
	added := []restCommit{}
	for _, commit := range pending {
		identity, ok := c.patchIdentity(commit.SHA)
		if !ok {
			omit("patch_unavailable", "a current commit patch was unavailable or truncated")
			return
		}
		if identities[identity] > 0 {
			identities[identity]--
			continue
		}
		added = append(added, commit)
	}
	c.result.NewCommits = commitRefs(added)
	c.complete(key)
}

func (c *collector) patchIdentity(sha string) (string, bool) {
	var detail restCommitDetail
	if !c.rest(SourceCommits, c.repoEndpoint()+"/commits/"+sha, &detail, false) {
		return "", false
	}
	c.addItems(SourceCommits, 1)
	if len(detail.Files) >= compareFileLimit || len(detail.Parents) > 1 {
		return "", false
	}
	files := append([]restFile(nil), detail.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	hash := sha256.New()
	for _, file := range files {
		patch, ok := effectivePatch(file)
		if !ok {
			return "", false
		}
		hunks, ok := parseHunks(patch)
		if !ok {
			return "", false
		}
		// Status and source path are part of every file's identity, so a
		// rename from a different source with identical edits does not match.
		hash.Write([]byte(file.Filename + "\x00" + file.Status + "\x00" + file.PreviousFilename + "\x00"))
		if len(hunks) == 0 {
			// An empty patch (for example a pure rename) carries no content,
			// so its blob identifies the change.
			hash.Write([]byte(file.SHA + "\x00"))
		}
		for _, hunk := range hunks {
			hash.Write([]byte(hunk.body + "\x01"))
		}
		hash.Write([]byte{'\x02'})
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

// ---- patch comparison ----

type hunk struct {
	header string
	body   string
}

// parseHunks splits a unified-diff patch into hunks. A hunk's identity is its
// body (context, added, and removed lines) without the @@ header.
func parseHunks(patch string) ([]hunk, bool) {
	if patch == "" {
		return nil, true
	}
	var hunks []hunk
	var body []string
	flush := func() {
		if len(hunks) > 0 {
			hunks[len(hunks)-1].body = strings.Join(body, "\n")
		}
		body = body[:0]
	}
	for _, line := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			hunks = append(hunks, hunk{header: line})
			continue
		}
		if len(hunks) == 0 {
			return nil, false
		}
		body = append(body, line)
	}
	flush()
	return hunks, true
}

// effectivePatch returns a file's patch. GitHub omits the patch for binary and
// oversized files; a pure rename has no patch and no changes.
func effectivePatch(file restFile) (string, bool) {
	if file.Patch != nil {
		return *file.Patch, true
	}
	if file.Status == "renamed" && file.Changes == 0 {
		return "", true
	}
	return "", false
}

var statusOrder = map[string]int{VerdictChanged: 0, FileAdded: 1, FileRemoved: 2, VerdictUnknown: 3, VerdictMovedOnly: 4, VerdictUnchanged: 5}

func compareFiles(baselineFiles, headFiles []restFile, maxHunks int) []FileDelta {
	baseline := map[string]restFile{}
	for _, file := range baselineFiles {
		baseline[file.Filename] = file
	}
	seen := map[string]bool{}
	files := []FileDelta{}
	for _, file := range headFiles {
		seen[file.Filename] = true
		old, exists := baseline[file.Filename]
		if !exists {
			files = append(files, sideOnlyFile(file, FileAdded, maxHunks))
			continue
		}
		files = append(files, compareFile(old, file, maxHunks))
	}
	for _, file := range baselineFiles {
		if seen[file.Filename] {
			continue
		}
		files = append(files, sideOnlyFile(file, FileRemoved, maxHunks))
	}
	sortFiles(files)
	return files
}

func sortFiles(files []FileDelta) {
	sort.SliceStable(files, func(i, j int) bool {
		if statusOrder[files[i].Status] != statusOrder[files[j].Status] {
			return statusOrder[files[i].Status] < statusOrder[files[j].Status]
		}
		return files[i].Path < files[j].Path
	})
}

// sideOnlyFile describes a file that entered (added) or left (removed) the PR
// diff. Without a usable patch its hunks are unknown, so the file is unknown
// like a changed file whose patch is missing.
func sideOnlyFile(file restFile, status string, maxHunks int) FileDelta {
	delta := FileDelta{Path: file.Filename, Status: status}
	patch, ok := effectivePatch(file)
	if !ok {
		delta.Status = VerdictUnknown
		delta.Reason = "GitHub omitted the patch (binary or too large)"
		return delta
	}
	hunks, ok := parseHunks(patch)
	if !ok {
		delta.Status = VerdictUnknown
		delta.Reason = "patch could not be parsed into hunks"
		return delta
	}
	if status == FileAdded {
		delta.ChangedHunks, delta.HunksOmitted = pairHunks(nil, hunks, maxHunks)
	} else {
		delta.ChangedHunks, delta.HunksOmitted = pairHunks(hunks, nil, maxHunks)
	}
	return delta
}

func compareFile(old, current restFile, maxHunks int) FileDelta {
	delta := FileDelta{Path: current.Filename}
	oldPatch, oldOK := effectivePatch(old)
	newPatch, newOK := effectivePatch(current)
	if !oldOK || !newOK {
		delta.Status = VerdictUnknown
		delta.Reason = "GitHub omitted the patch (binary or too large)"
		return delta
	}
	sameShape := old.Status == current.Status && old.PreviousFilename == current.PreviousFilename
	if oldPatch == newPatch && sameShape && (oldPatch != "" || old.SHA == current.SHA) {
		delta.Status = VerdictUnchanged
		return delta
	}
	oldHunks, oldParsed := parseHunks(oldPatch)
	newHunks, newParsed := parseHunks(newPatch)
	if !oldParsed || !newParsed {
		delta.Status = VerdictUnknown
		delta.Reason = "patch could not be parsed into hunks"
		return delta
	}
	changes, omitted := pairHunks(oldHunks, newHunks, maxHunks)
	switch {
	case len(changes) == 0 && omitted == 0 && sameShape && (oldPatch != "" || old.SHA == current.SHA):
		delta.Status = VerdictMovedOnly
	default:
		delta.Status = VerdictChanged
		delta.ChangedHunks, delta.HunksOmitted = changes, omitted
		if len(changes) == 0 && omitted == 0 {
			delta.Reason = "file status, rename source, or content identity changed"
		}
	}
	return delta
}

// pairHunks removes hunks whose bodies appear on both sides, then pairs the
// remaining baseline and head hunks in order. Each baseline hunk matches at
// most one head hunk. A head hunk first matches a baseline hunk with the same
// header and body; otherwise it matches the same-body baseline hunk whose old
// start line is nearest, so the unmatched instance of a repeated body is the
// one that was actually dropped.
func pairHunks(oldHunks, newHunks []hunk, maxHunks int) ([]HunkChange, int) {
	matchedOld := make([]bool, len(oldHunks))
	matchedNew := make([]bool, len(newHunks))
	for j, item := range newHunks {
		for i, old := range oldHunks {
			if !matchedOld[i] && old.header == item.header && old.body == item.body {
				matchedOld[i], matchedNew[j] = true, true
				break
			}
		}
	}
	for j, item := range newHunks {
		if matchedNew[j] {
			continue
		}
		best := -1
		for i, old := range oldHunks {
			if matchedOld[i] || old.body != item.body {
				continue
			}
			if best < 0 || lineDistance(old, item) < lineDistance(oldHunks[best], item) {
				best = i
			}
		}
		if best >= 0 {
			matchedOld[best], matchedNew[j] = true, true
		}
	}
	var unmatchedOld, unmatchedNew []hunk
	for i, item := range oldHunks {
		if !matchedOld[i] {
			unmatchedOld = append(unmatchedOld, item)
		}
	}
	for j, item := range newHunks {
		if !matchedNew[j] {
			unmatchedNew = append(unmatchedNew, item)
		}
	}
	count := max(len(unmatchedOld), len(unmatchedNew))
	changes := []HunkChange{}
	for index := 0; index < count && index < maxHunks; index++ {
		var change HunkChange
		if index < len(unmatchedOld) {
			change.OldHeader = unmatchedOld[index].header
		}
		if index < len(unmatchedNew) {
			change.NewHeader = unmatchedNew[index].header
		}
		changes = append(changes, change)
	}
	return changes, count - len(changes)
}

// lineDistance is the distance between two hunks' old-file start lines.
func lineDistance(left, right hunk) int {
	distance := oldStart(left.header) - oldStart(right.header)
	if distance < 0 {
		return -distance
	}
	return distance
}

// oldStart reads the old-file start line from an "@@ -a,b +c,d @@" header,
// or 0 when the header has no parsable range.
func oldStart(header string) int {
	rest, ok := strings.CutPrefix(header, "@@ -")
	if !ok {
		return 0
	}
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		return 0
	}
	start, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return start
}

func verdict(files []FileDelta) string {
	result := VerdictUnchanged
	for _, file := range files {
		switch file.Status {
		case VerdictUnknown:
			return VerdictUnknown
		case VerdictChanged, FileAdded, FileRemoved:
			result = VerdictChanged
		case VerdictMovedOnly:
			if result == VerdictUnchanged {
				result = VerdictMovedOnly
			}
		}
	}
	return result
}

// ---- file modes ----

// modeLookupBatch bounds the file(path:) lookups sent in one GraphQL request.
const modeLookupBatch = 200

type modeKey struct{ commit, path string }

// modeChange is the mode change a side's PR diff makes to a file, as the mode
// at the merge base and at the side's commit. Both are zero when the PR leaves
// the mode alone, and a zero mode means the file is absent.
type modeChange struct{ from, to int }

// checkModes compares the file mode change of each unchanged or moved_only
// file. Compare file objects carry no mode, so the modes come from GraphQL
// Commit.file at each side's merge base and commit. Comparing the change, and
// not the head and baseline modes directly, keeps a mode change that arrived
// through the base out of the PR delta. A file whose mode change differs
// becomes changed with mode_changed. When a lookup fails or exceeds the
// request budget, the affected files become unknown.
func (c *collector) checkModes(files []FileDelta, baselineSide, headSide compareSide) {
	if baselineSide.head == headSide.head && baselineSide.mergeBase() == headSide.mergeBase() {
		return
	}
	sides := []map[string]restFile{indexFiles(baselineSide.response.Files), indexFiles(headSide.response.Files)}
	commits := []compareSide{baselineSide, headSide}
	var candidates []int
	var keys []modeKey
	seen := map[modeKey]bool{}
	for index, file := range files {
		if file.Status != VerdictUnchanged && file.Status != VerdictMovedOnly {
			continue
		}
		candidates = append(candidates, index)
		for n, side := range commits {
			from, to := modeKeys(sides[n][file.Path], side)
			for _, key := range []*modeKey{from, to} {
				if key != nil && !seen[*key] {
					seen[*key] = true
					keys = append(keys, *key)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	modes, ok := c.fileModes(keys)
	for _, index := range candidates {
		file := &files[index]
		if !ok {
			file.Status = VerdictUnknown
			file.Reason = "file mode could not be read"
			continue
		}
		var changes [2]modeChange
		for n, side := range commits {
			from, to := modeKeys(sides[n][file.Path], side)
			changes[n] = modeChange{modeAt(modes, from), modeAt(modes, to)}
			if changes[n].from == changes[n].to {
				changes[n] = modeChange{}
			}
		}
		if changes[0] != changes[1] {
			file.Status = VerdictChanged
			file.ModeChanged = true
			file.Reason = "file mode change differs: baseline side " + changes[0].String() + ", head side " + changes[1].String()
		}
	}
}

func indexFiles(files []restFile) map[string]restFile {
	index := make(map[string]restFile, len(files))
	for _, file := range files {
		index[file.Filename] = file
	}
	return index
}

// modeKeys returns the lookups for a file's mode at the side's merge base
// (its previous path for a rename or copy) and at the side's commit. A file
// that the diff adds has no merge-base lookup, and a removed file has no
// commit lookup, so every lookup names a path that exists.
func modeKeys(file restFile, side compareSide) (from, to *modeKey) {
	if file.Status != "added" {
		path := file.Filename
		if file.PreviousFilename != "" {
			path = file.PreviousFilename
		}
		from = &modeKey{side.mergeBase(), path}
	}
	if file.Status != "removed" {
		to = &modeKey{side.head, file.Filename}
	}
	return from, to
}

func modeAt(modes map[modeKey]int, key *modeKey) int {
	if key == nil {
		return 0
	}
	return modes[*key]
}

func (change modeChange) String() string {
	if change == (modeChange{}) {
		return "no mode change"
	}
	return octalMode(change.from) + " -> " + octalMode(change.to)
}

func octalMode(mode int) string {
	if mode == 0 {
		return "absent"
	}
	return strconv.FormatInt(int64(mode), 8)
}

// fileModes reads each key's mode in batches of modeLookupBatch lookups per
// GraphQL request. Paths and commits travel as variables.
func (c *collector) fileModes(keys []modeKey) (map[modeKey]int, bool) {
	modes := map[modeKey]int{}
	for start := 0; start < len(keys); start += modeLookupBatch {
		batch := keys[start:min(start+modeLookupBatch, len(keys))]
		query, variables, aliases := modeQuery(c.ref.Owner, c.ref.Name, batch)
		var data struct {
			Repository map[string]map[string]*struct {
				Mode *int `json:"mode"`
			} `json:"repository"`
		}
		if !c.graphQL(SourcePRDiff, query, variables, &data, false) {
			return nil, false
		}
		for index, key := range batch {
			entry := data.Repository[aliases[index][0]][aliases[index][1]]
			if entry == nil || entry.Mode == nil || *entry.Mode == 0 {
				c.fail(SourcePRDiff, "invalid_response", "file mode lookup omitted a file")
				return nil, false
			}
			modes[key] = *entry.Mode
		}
	}
	return modes, true
}

// modeQuery builds one query with a Commit alias per commit and a file alias
// per lookup. aliases[i] names the commit and file aliases of keys[i].
func modeQuery(owner, name string, keys []modeKey) (string, map[string]any, [][2]string) {
	variables := map[string]any{"owner": owner, "name": name}
	declarations := []string{"$owner: String!", "$name: String!"}
	commitAlias := map[string]string{}
	var commitOrder []string
	fields := map[string][]string{}
	aliases := make([][2]string, len(keys))
	for index, key := range keys {
		alias, ok := commitAlias[key.commit]
		if !ok {
			alias = "c" + strconv.Itoa(len(commitOrder))
			commitAlias[key.commit] = alias
			commitOrder = append(commitOrder, key.commit)
			variables[alias] = key.commit
			declarations = append(declarations, "$"+alias+": GitObjectID!")
		}
		file, path := "f"+strconv.Itoa(index), "p"+strconv.Itoa(index)
		variables[path] = key.path
		declarations = append(declarations, "$"+path+": String!")
		fields[alias] = append(fields[alias], file+": file(path: $"+path+") { mode }")
		aliases[index] = [2]string{alias, file}
	}
	var body strings.Builder
	for _, commit := range commitOrder {
		alias := commitAlias[commit]
		fmt.Fprintf(&body, "    %s: object(oid: $%s) { ... on Commit { %s } }\n", alias, alias, strings.Join(fields[alias], " "))
	}
	query := "query(" + strings.Join(declarations, ", ") + ") {\n  repository(owner: $owner, name: $name) {\n" + body.String() + "  }\n}"
	return query, variables, aliases
}

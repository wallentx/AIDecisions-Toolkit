package prdelta

import (
	"crypto/sha256"
	"encoding/hex"
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
	diff.Verdict = verdict(diff.Files)
	for _, file := range diff.Files {
		diff.Counts[file.Status]++
	}
	c.addItems(key, len(diff.Files))
	if diff.Verdict == VerdictUnknown {
		diff.Reason = "at least one file patch was omitted by GitHub (binary or too large) or could not be parsed"
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
		hash.Write([]byte(file.Filename + "\x00"))
		if len(hunks) == 0 {
			// An empty patch (for example a pure rename) carries no content,
			// so its status, source path, and blob identify the change.
			hash.Write([]byte(file.Status + "\x00" + file.PreviousFilename + "\x00" + file.SHA + "\x00"))
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
	sort.SliceStable(files, func(i, j int) bool {
		if statusOrder[files[i].Status] != statusOrder[files[j].Status] {
			return statusOrder[files[i].Status] < statusOrder[files[j].Status]
		}
		return files[i].Path < files[j].Path
	})
	return files
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

// pairHunks removes hunks whose bodies appear on both sides (as a multiset),
// then pairs the remaining baseline and head hunks in order.
func pairHunks(oldHunks, newHunks []hunk, maxHunks int) ([]HunkChange, int) {
	remaining := map[string]int{}
	for _, item := range oldHunks {
		remaining[item.body]++
	}
	var unmatchedNew []hunk
	for _, item := range newHunks {
		if remaining[item.body] > 0 {
			remaining[item.body]--
			continue
		}
		unmatchedNew = append(unmatchedNew, item)
	}
	var unmatchedOld []hunk
	for _, item := range oldHunks {
		if remaining[item.body] > 0 {
			remaining[item.body]--
			unmatchedOld = append(unmatchedOld, item)
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

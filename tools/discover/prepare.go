package discover

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const SchemaVersion = 2
const maxInputBytes = 64 << 20

var resourcePattern = regexp.MustCompile(`\[(github-(?:pr|run|issue)-[a-f0-9]{24})\]`)
var placeholderPattern = regexp.MustCompile(`\[(?:path-[0-9a-f]+|github-[a-z]+-[0-9a-f]+|private-key|authorization|secret|token|url|email|path|opaque)\]`)
var wordPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*(?:['’][A-Za-z]+)?`)
var fencedCodePattern = regexp.MustCompile("(?s)```.*?(?:```|$)")
var labelPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,47}$`)
var stopWords = func() map[string]bool {
	m := make(map[string]bool)
	for _, word := range strings.Fields("a an the and or but if as at by for in of on to up out off over under not no nor so than too very am is are be being was has had having do does did doing can may might must shall get gets got getting let lets us me my myself our ours ourselves you yours yourself yourselves he him his himself she her hers herself it its itself we they theirs themselves who whom whose why how all any both each few most other such same own here again once ever never always already still yet now really quite rather even much many less least well yes yeah yep nope ok hello hi hey sorry alright good great nice fine right actually basically probably perhaps maybe simply literally obviously essentially seems seem think know see look like dont doesnt didnt isnt arent wasnt werent havent hasnt hadnt wont wouldnt couldnt shouldnt cant cannot im ive ill id youre youve youll youd hes shes its were theyre theyve thatll thats theres whats whos wasnt arent couldve shouldve wouldve something anything everything nothing someone anyone everyone nobody stuff things thing because since while although though unless until whether whatever whenever wherever however anyway anyways also else another enough around away back down through across between within without either neither together along almost especially exactly example examples doing going went come comes came take takes taken give gives given tell tells told say says said ask asks asked use uses via per thus hence etc okay that this with from have your will would could should what when where which there their about please thanks thank done okay sure just then them they these those into some more only also after before been were using used make need want work code test tests testing user assistant tool result message json true false null string return function const import export class public private static package error errors info debug path opaque token secret email authorization github issue") {
		m[word] = true
	}
	return m
}()

type inputRow struct {
	Kind      string          `json:"kind"`
	Message   string          `json:"message"`
	Project   string          `json:"project"`
	SessionID string          `json:"session_id"`
	Timestamp string          `json:"timestamp"`
	Provider  string          `json:"provider"`
	Record    json.RawMessage `json:"record"`
}

// Prepare streams a normalized JSON array and retains only sanitized evidence.
// Incomplete valid prefixes are returned with explicit coverage warnings.
func Prepare(reader io.Reader, opts PrepareOptions) (*Bundle, error) {
	if len(opts.Salt) < 16 {
		return nil, errors.New("preparation requires an installation salt of at least 16 bytes")
	}
	if opts.Days < 0 || opts.MaxRecords < 0 || opts.MaxRecordBytes < 0 {
		return nil, errors.New("invalid preparation limits")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.Days == 0 {
		opts.Days = 30
	}
	if opts.MaxRecords == 0 {
		opts.MaxRecords = 100000
	}
	if opts.MaxRecordBytes == 0 {
		opts.MaxRecordBytes = 32768
	}
	if opts.MaxRecordBytes > 1<<20 {
		opts.MaxRecordBytes = 1 << 20
	}
	since := opts.Now.AddDate(0, 0, -opts.Days)
	bundle := &Bundle{SchemaVersion: SchemaVersion, PreparedAt: opts.Now.UTC().Format(time.RFC3339Nano), Records: []Record{}, Groups: []Group{}}
	coverage := &bundle.Coverage
	coverage.Complete = true
	coverage.RequestedSince = since.UTC().Format(time.RFC3339Nano)
	coverage.RequestedUntil = opts.Now.UTC().Format(time.RFC3339Nano)
	coverage.Warnings = []string{"Redaction is best effort; residual private information may remain. The current agent must screen excerpts before delegation or sharing.", "Groups are overlapping lexical leads, not semantic findings; do not add their counts."}
	limited := &io.LimitedReader{R: reader, N: maxInputBytes + 1}
	decoder := json.NewDecoder(limited)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('[') {
		return nil, errors.New("normalized input must be a JSON array")
	}
	seen := make(map[string]bool)
	stopped := false
	for decoder.More() {
		if coverage.InputRecords >= opts.MaxRecords {
			warn(coverage, "Input record limit reached; later rows were not examined.")
			stopped = true
			break
		}
		coverage.InputRecords++
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			coverage.RejectedRecords++
			warn(coverage, "Malformed or oversized input; only the valid prefix was examined.")
			stopped = true
			break
		}
		if len(raw) > 2<<20 {
			coverage.RejectedRecords++
			warn(coverage, "An oversized input row was rejected.")
			continue
		}
		var row inputRow
		if json.Unmarshal(raw, &row) != nil {
			coverage.RejectedRecords++
			warn(coverage, "Rows with invalid normalized fields were rejected.")
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || strings.TrimSpace(row.Project) == "" || strings.TrimSpace(row.SessionID) == "" || strings.TrimSpace(row.Message) == "" || !labelPattern.MatchString(row.Kind) {
			coverage.RejectedRecords++
			warn(coverage, "Rows missing valid identity, timestamp, kind, or message were rejected.")
			continue
		}
		if row.Provider == "" {
			row.Provider = opts.Provider
		}
		if row.Provider == "" || row.Provider == "all" {
			row.Provider = "unknown"
		}
		if !labelPattern.MatchString(row.Provider) || Redact(row.Provider) != row.Provider || Redact(row.Kind) != row.Kind {
			coverage.RejectedRecords++
			warn(coverage, "Rows with invalid provider labels were rejected.")
			continue
		}
		if timestamp.Before(since) || timestamp.After(opts.Now) || (!opts.AllProjects && row.Project != opts.Project) || (opts.Provider != "" && opts.Provider != "all" && row.Provider != opts.Provider) {
			coverage.RejectedRecords++
			addWarning(coverage, "Rows outside the requested provider, project, or date scope were filtered.")
			continue
		}
		text := redact(row.Message, opts.Salt)
		record := Record{Provider: row.Provider, ProjectID: alias(opts.Salt, "project", row.Project), SessionID: alias(opts.Salt, "session", row.Provider+"\x00"+row.SessionID), Timestamp: timestamp.UTC().Format(time.RFC3339Nano), Kind: row.Kind, Text: text, Redacted: text != row.Message, ResourceIDs: []string{}}
		if len(text) > opts.MaxRecordBytes {
			end := opts.MaxRecordBytes
			for end > 0 && !utf8.ValidString(text[:end]) {
				end--
			}
			record.Text = text[:end]
			record.Truncated = true
			warn(coverage, "Long evidence text was truncated after redaction.")
		}
		identity := string(row.Record)
		if len(identity) > 256 {
			identity = ""
		}
		identity = redact(identity, opts.Salt)
		record.ID = alias(opts.Salt, "evidence", strings.Join([]string{record.Provider, record.ProjectID, record.SessionID, record.Timestamp, record.Kind, identity, text}, "\x00"))
		if seen[record.ID] {
			coverage.DuplicateRecords++
			continue
		}
		seen[record.ID] = true
		resources := make(map[string]bool)
		for _, match := range resourcePattern.FindAllStringSubmatch(record.Text, -1) {
			resources[match[1]] = true
		}
		for resource := range resources {
			record.ResourceIDs = append(record.ResourceIDs, resource)
		}
		sort.Strings(record.ResourceIDs)
		if record.Redacted {
			coverage.RedactedRecords++
		}
		if record.Truncated {
			coverage.TruncatedRecords++
		}
		bundle.Records = append(bundle.Records, record)
	}
	if !stopped {
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			warn(coverage, "Input array was not complete.")
		} else {
			var trailing json.RawMessage
			if decoder.Decode(&trailing) != io.EOF {
				warn(coverage, "Unexpected trailing input was ignored.")
			}
		}
	}
	if limited.N <= 0 {
		warn(coverage, "Input byte limit reached; later bytes were not examined.")
	}
	sort.SliceStable(bundle.Records, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, bundle.Records[i].Timestamp)
		b, _ := time.Parse(time.RFC3339Nano, bundle.Records[j].Timestamp)
		return a.Before(b)
	})
	sessions, projects := make(map[string]bool), make(map[string]bool)
	for _, record := range bundle.Records {
		sessions[record.SessionID] = true
		projects[record.ProjectID] = true
		if coverage.ObservedSince == "" {
			coverage.ObservedSince = record.Timestamp
		}
		coverage.ObservedUntil = record.Timestamp
	}
	coverage.RetainedRecords = len(bundle.Records)
	coverage.Sessions = len(sessions)
	coverage.Projects = len(projects)
	detectReplays(bundle, opts.Salt)
	bundle.Groups = buildGroups(bundle.Records, opts.Salt)
	return bundle, nil
}

func addWarning(c *Coverage, message string) {
	for _, old := range c.Warnings {
		if old == message {
			return
		}
	}
	c.Warnings = append(c.Warnings, message)
}
func warn(c *Coverage, message string) { c.Complete = false; addWarning(c, message) }

// topicTerms preserves the first occurrence of informative words, including
// short acronyms, without letting pasted fenced code displace a concise request.
func topicTerms(text string) []string {
	text = strings.TrimSpace(placeholderPattern.ReplaceAllString(fencedCodePattern.ReplaceAllString(text, " "), " "))
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		return nil
	}
	terms := []string{}
	seen := make(map[string]bool)
	for _, raw := range wordPattern.FindAllString(text, -1) {
		word := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(raw, "'", ""), "’", ""))
		if len(word) < 2 || len(word) > 32 || stopWords[word] || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
		if len(terms) == 12 {
			break
		}
	}
	return terms
}

func meaningful(text string) bool { return len(topicTerms(text)) > 0 }

// A matching three-message window is only a possible replay. Evidence stays
// accessible while affected records are excluded from independent lead counts.
func detectReplays(bundle *Bundle, salt []byte) {
	sequences := make(map[string][]int)
	order := []string{}
	for index, record := range bundle.Records {
		if !meaningful(record.Text) {
			continue
		}
		key := record.ProjectID + "\x00" + record.SessionID
		if _, exists := sequences[key]; !exists {
			order = append(order, key)
		}
		sequences[key] = append(sequences[key], index)
	}
	type window struct {
		session string
		records [3]int
	}
	seen := make(map[string]window)
	for _, key := range order {
		sequence := sequences[key]
		for i := 0; i+2 < len(sequence); i++ {
			indices := [3]int{sequence[i], sequence[i+1], sequence[i+2]}
			texts := []string{}
			length := 0
			for _, index := range indices {
				record := bundle.Records[index]
				normalized := strings.Join(strings.Fields(record.Text), " ")
				texts = append(texts, record.Kind+"\x00"+normalized)
				length += len(normalized)
			}
			if length < 180 {
				continue
			}
			record := bundle.Records[indices[0]]
			fingerprint := alias(salt, "replay", record.ProjectID+"\x00"+strings.Join(texts, "\x01"))
			prior, exists := seen[fingerprint]
			if exists && prior.session != record.SessionID {
				for j, index := range indices {
					if bundle.Records[index].ReplayOf == "" {
						bundle.Records[index].ReplayOf = bundle.Records[prior.records[j]].ID
						bundle.Coverage.PossibleReplayRecords++
					}
				}
			} else if !exists {
				seen[fingerprint] = window{record.SessionID, indices}
			}
		}
	}
	if bundle.Coverage.PossibleReplayRecords > 0 {
		addWarning(&bundle.Coverage, "Matching multi-message sequences are possible replays, not proven duplicates; they remain retrievable but do not increase conservative lead counts.")
	}
}

func buildGroups(records []Record, salt []byte) []Group {
	index := make(map[string][]int)
	for i, record := range records {
		terms := topicTerms(record.Text)
		for _, resource := range record.ResourceIDs {
			if strings.HasPrefix(resource, "github-pr-") {
				terms = append(terms, "resource:github-pr")
			}
			if strings.HasPrefix(resource, "github-run-") {
				terms = append(terms, "resource:github-run")
			}
			if strings.HasPrefix(resource, "github-issue-") {
				terms = append(terms, "resource:github-issue")
			}
		}
		added := map[string]bool{}
		for _, term := range terms {
			if !added[term] {
				index[term] = append(index[term], i)
				added[term] = true
			}
		}
	}
	groups := []Group{}
	for term, indices := range index {
		if len(indices) < 2 {
			continue
		}
		group := Group{ID: alias(salt, "lead", term), Label: term, Terms: []string{term}, RecordIDs: []string{}}
		sessions, projects, incidents := map[string]bool{}, map[string]bool{}, map[string]bool{}
		fallbackSessions, resourceSessions := map[string]bool{}, map[string]bool{}
		for _, index := range indices {
			record := records[index]
			group.RecordIDs = append(group.RecordIDs, record.ID)
			if record.ReplayOf != "" {
				continue
			}
			sessions[record.SessionID] = true
			projects[record.ProjectID] = true
			if len(record.ResourceIDs) > 0 {
				resourceSessions[record.SessionID] = true
				for _, resource := range record.ResourceIDs {
					incidents[resource] = true
				}
			} else {
				// Without a resource identity, several messages may be one incident.
				fallbackSessions[record.SessionID] = true
			}
		}
		for session := range fallbackSessions {
			if !resourceSessions[session] {
				incidents["session:"+session] = true
			}
		}
		group.Records = len(group.RecordIDs)
		group.Sessions = len(sessions)
		group.Projects = len(projects)
		group.Incidents = len(incidents)
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		if a.Projects != b.Projects {
			return a.Projects > b.Projects
		}
		if a.Incidents != b.Incidents {
			return a.Incidents > b.Incidents
		}
		if a.Records != b.Records {
			return a.Records > b.Records
		}
		return a.Label < b.Label
	})
	// Keep every group's record IDs for deterministic later evidence paging.
	return groups
}

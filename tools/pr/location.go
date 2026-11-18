package pr

import "encoding/json"

// groupLocations receives findings in thread-ID order. The side identifies
// the preimage or postimage of the given diff revision. Outdated findings
// carry original coordinates instead of coordinates from the current diff.
func groupLocations(findings []Finding, maxBytes int, incomplete func(string)) {
	type location struct {
		path, commit, side string
		line               int
	}
	type member struct {
		id   string
		cost int
	}
	groups := make(map[location][]member)
	keyFor := func(f Finding) (location, bool) {
		if f.Line == nil || f.Path == "" || f.Commit == "" || (f.DiffSide != "LEFT" && f.DiffSide != "RIGHT") {
			return location{}, false
		}
		return location{f.Path, f.Commit, f.DiffSide, *f.Line}, true
	}
	for _, f := range findings {
		if key, ok := keyFor(f); ok {
			// JSON escaping can expand an ID. Measure it only once regardless
			// of how many peer lists refer to it; allow one byte for a comma.
			id, _ := json.Marshal(f.ThreadID)
			groups[key] = append(groups[key], member{f.ThreadID, len(id) + 1})
		}
	}

	const maxPerThread = 100
	remaining := maxBytes / 8
	for i := range findings {
		f := &findings[i]
		key, ok := keyFor(*f)
		if !ok {
			continue
		}
		peers := groups[key]
		if len(peers)-1 > maxPerThread {
			incomplete("same_location_peer_limit")
			f.SameLocationTruncated = true
		}
		for _, peer := range peers {
			if peer.id == f.ThreadID {
				continue
			}
			if len(f.SameLocationAs) == maxPerThread {
				break
			}
			if peer.cost > remaining {
				f.SameLocationTruncated = true
				incomplete("same_location_byte_limit")
				break
			}
			f.SameLocationAs = append(f.SameLocationAs, peer.id)
			remaining -= peer.cost
		}
	}
}

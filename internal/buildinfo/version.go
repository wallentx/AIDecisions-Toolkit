package buildinfo

// Version identifies the fallback suite version when neither RELEASE_TAG nor
// a reachable v* Git tag is present; individual tools version separately.
var Version = "0.1.0"

type Info struct {
	Command        string `json:"command"`
	ToolVersion    string `json:"tool_version"`
	ReleaseVersion string `json:"release_version"`
}

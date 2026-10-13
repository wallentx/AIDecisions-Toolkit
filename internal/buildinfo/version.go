package buildinfo

// Version identifies the tested suite snapshot; individual tools version separately.
// The native builder sets this from VERSION using -ldflags.
var Version = "0.6.0"

type Info struct {
	Command        string `json:"command"`
	ToolVersion    string `json:"tool_version"`
	ReleaseVersion string `json:"release_version"`
}

package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestDiscoversNewToolsFromTheSameManifests(t *testing.T) {
	files := fstest.MapFS{}
	root := t.TempDir()
	for _, id := range []string{"diff", "logs"} {
		tool := Tool{ID: id, Command: "AID." + id, Version: "0.1.0", Description: "test", Package: "./cmd/AID." + id}
		data, _ := json.Marshal(tool)
		base := "tools/" + id + "/"
		files[base+"tool.json"] = &fstest.MapFile{Data: data}
		files[base+"SKILL.md"] = &fstest.MapFile{Data: []byte("test skill")}
		os.MkdirAll(filepath.Join(root, "tools", id), 0755)
		os.WriteFile(filepath.Join(root, "tools", id, "tool.json"), data, 0644)
		os.WriteFile(filepath.Join(root, "tools", id, "SKILL.md"), []byte("test skill"), 0644)
	}
	embedded, err := fromFS(files)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := FromDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) != 2 || !reflect.DeepEqual(embedded, disk) {
		t.Fatalf("registry differs: %+v %+v", embedded, disk)
	}
}

func TestRejectsEscapingPackage(t *testing.T) {
	data := []byte(`{"id":"bad","command":"AID.bad","version":"0.1.0","description":"bad","package":"../../other"}`)
	if _, err := parse(data, []byte("skill")); err == nil {
		t.Fatal("accepted escaped package path")
	}
}

func TestCollectorBlockIsOptionalAndValidated(t *testing.T) {
	plain, err := parse([]byte(`{"id":"pr","command":"AID.pr","version":"0.1.0","description":"d","package":"./cmd/AID.pr"}`), []byte("skill"))
	if err != nil || plain.Collector != nil {
		t.Fatalf("plain manifest = %+v, %v", plain, err)
	}
	valid := `{"id":"queue","command":"AID.queue","version":"0.1.0","description":"d","package":"./cmd/AID.queue",
	  "collector":{"name":"github_queue","description":"q","args":["--json","--no-delta"],"workspace_flag":"--workspace","timeout_flag":"--timeout","ttl_seconds":60,"timeout_seconds":60}}`
	tool, err := parse([]byte(valid), []byte("skill"))
	if err != nil || tool.Collector == nil || tool.Collector.Name != "github_queue" || tool.Collector.TTLSeconds != 60 || tool.Collector.TimeoutFlag != "--timeout" {
		t.Fatalf("collector manifest = %+v, %v", tool.Collector, err)
	}
	for name, collector := range map[string]string{
		"bad name":         `{"name":"Bad-Name","description":"q","args":["--json"],"ttl_seconds":1,"timeout_seconds":1}`,
		"no description":   `{"name":"q","description":"","args":["--json"],"ttl_seconds":1,"timeout_seconds":1}`,
		"no json":          `{"name":"q","description":"q","args":["--text"],"ttl_seconds":1,"timeout_seconds":1}`,
		"shell arg":        `{"name":"q","description":"q","args":["--json","--x=$(id)"],"ttl_seconds":1,"timeout_seconds":1}`,
		"positional":       `{"name":"q","description":"q","args":["--json","value"],"ttl_seconds":1,"timeout_seconds":1}`,
		"bad flag":         `{"name":"q","description":"q","args":["--json"],"workspace_flag":"-w x","ttl_seconds":1,"timeout_seconds":1}`,
		"bad timeout flag": `{"name":"q","description":"q","args":["--json"],"timeout_flag":"--timeout=1","ttl_seconds":1,"timeout_seconds":1}`,
		"same flags":       `{"name":"q","description":"q","args":["--json"],"workspace_flag":"--x","timeout_flag":"--x","ttl_seconds":1,"timeout_seconds":1}`,
		"zero timeout":     `{"name":"q","description":"q","args":["--json"],"ttl_seconds":1,"timeout_seconds":0}`,
		"negative ttl":     `{"name":"q","description":"q","args":["--json"],"ttl_seconds":-1,"timeout_seconds":1}`,
	} {
		data := `{"id":"queue","command":"AID.queue","version":"0.1.0","description":"d","package":"./cmd/AID.queue","collector":` + collector + `}`
		if _, err := parse([]byte(data), []byte("skill")); err == nil {
			t.Errorf("%s: accepted invalid collector", name)
		}
	}
}

func TestDuplicateCollectorNamesRejected(t *testing.T) {
	files := fstest.MapFS{}
	for _, id := range []string{"one", "two"} {
		data := `{"id":"` + id + `","command":"AID.` + id + `","version":"0.1.0","description":"d","package":"./cmd/AID.` + id + `",
		  "collector":{"name":"same","description":"q","args":["--json"],"ttl_seconds":1,"timeout_seconds":1}}`
		files["tools/"+id+"/tool.json"] = &fstest.MapFile{Data: []byte(data)}
		files["tools/"+id+"/SKILL.md"] = &fstest.MapFile{Data: []byte("skill")}
	}
	if _, err := fromFS(files); err == nil {
		t.Fatal("accepted duplicate collector names")
	}
}

func TestEmbeddedCatalogCollectors(t *testing.T) {
	tools, err := All()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Tool{}
	for _, tool := range tools {
		byID[tool.ID] = tool
	}
	if queue := byID["review-queue"].Collector; queue == nil || queue.Name != "github_review_queue" || queue.WorkspaceFlag != "--workspace" {
		t.Errorf("review-queue collector = %+v", queue)
	}
	if byID["pr-delta"].Collector != nil {
		t.Error("pr-delta should not declare a collector")
	}
}

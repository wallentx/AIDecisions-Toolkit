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

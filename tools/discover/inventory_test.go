package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func inventoryFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func inventoryTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, "selected-codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "selected-claude"))
	t.Setenv("COPILOT_HOME", filepath.Join(home, "selected-copilot"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	return home
}

func TestInventoryUsesSelectedProviderAndMetadataOnly(t *testing.T) {
	home := inventoryTestHome(t)
	project := filepath.Join(home, "project")
	inventoryFixture(t, filepath.Join(home, "selected-codex", "skills", "alpha", "SKILL.md"), "---\nname: alpha\ndescription: Parse local records\n---\nBODY_PRIVATE_MARKER\n")
	inventoryFixture(t, filepath.Join(home, "selected-claude", "skills", "other", "SKILL.md"), "---\nname: OTHER_PROVIDER_MARKER\ndescription: other\n---\n")
	inventoryFixture(t, filepath.Join(project, ".agents", "skills", "beta", "SKILL.md"), "---\nname: beta\ndescription: Correlate logs\n---\n")
	inventoryFixture(t, filepath.Join(home, ".agents", "skills", "broken", "SKILL.md"), "---\nname: [INVALID_PRIVATE_MARKER\n---\n")
	inv, err := loadInventoryAtHome("codex", project, "", home)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete || inv.SkillsSeen != 3 {
		t.Fatalf("unexpected status: %+v", inv)
	}
	data, _ := json.Marshal(inv)
	for _, secret := range []string{"BODY_PRIVATE_MARKER", "OTHER_PROVIDER_MARKER", "INVALID_PRIVATE_MARKER", home} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("private data leaked: %q", secret)
		}
	}
	var skills []string
	var aidCount int
	for _, item := range inv.Items {
		if item.Source == "installed_skill" {
			skills = append(skills, item.Name)
		}
		if item.Source == "aid_tool" {
			aidCount++
		}
	}
	if !reflect.DeepEqual(skills, []string{"alpha", "beta"}) || aidCount == 0 {
		t.Fatalf("unexpected inventory: %+v", inv.Items)
	}
}

func TestInventorySymlinksAndDuplicates(t *testing.T) {
	home := inventoryTestHome(t)
	skill := filepath.Join(home, "shared", "actual")
	inventoryFixture(t, filepath.Join(skill, "SKILL.md"), "---\nname: shared\ndescription: same metadata\n---\n")
	root := filepath.Join(home, "selected-codex", "skills")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.Symlink(skill, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	inventoryFixture(t, filepath.Join(root, "three", "SKILL.md"), "---\nname: shared\ndescription: same metadata\n---\n")
	inventoryFixture(t, filepath.Join(root, "four", "SKILL.md"), "---\nname: shared\ndescription: different metadata\n---\n")
	inv, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete || inv.SkillsSeen != 3 {
		t.Fatalf("unexpected status: %+v", inv)
	}
	var skills []InventoryItem
	for _, item := range inv.Items {
		if item.Source == "installed_skill" {
			skills = append(skills, item)
		}
	}
	if len(skills) != 2 || skills[0].ID == skills[1].ID {
		t.Fatalf("duplicates not preserved correctly: %+v", skills)
	}
	again, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil || !reflect.DeepEqual(inv, again) {
		t.Fatal("inventory is not deterministic")
	}
}

func TestSkillMetadataBoundsAndBody(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"large body", "---\nname: tiny\ndescription: metadata\n---\n" + strings.Repeat("BODY_SECRET", 20000), true},
		{"large header", "---\nname: tiny\ndescription: " + strings.Repeat("x", inventoryMetadataLimit) + "\n---\n", false},
		{"unterminated", "---\nname: tiny\ndescription: metadata\n", false},
		{"no header", "name: tiny\ndescription: metadata\n", false},
		{"closing eof", "---\nname: tiny\ndescription: metadata\n---", true},
		{"crlf", "---\r\nname: tiny\r\ndescription: metadata\r\n---\r\nbody", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "SKILL.md")
			inventoryFixture(t, path, tc.content)
			name, description, err := readSkillMetadata(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
			if tc.valid && (name != "tiny" || description != "metadata") {
				t.Fatalf("unexpected fields: %q %q", name, description)
			}
			if !tc.valid && (name != "" || description != "") {
				t.Fatal("invalid metadata returned parsed values")
			}
		})
	}
}

func TestProvidedInventorySchemaAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", `[{"name":"alpha","description":"a"},{"name":"beta","description":"b"}]`, true},
		{"empty", "[]", true}, {"null", "null", false}, {"object", `{"name":"a","description":"b"}`, false},
		{"unknown", `[{"name":"a","description":"b","body":"SECRET"}]`, false},
		{"wrong type", `[{"name":1,"description":"b"}]`, false},
		{"missing", `[{"name":"a"}]`, false}, {"trailing", "[] true", false},
		{"oversized", strings.Repeat(" ", inventoryFileLimit+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inventory.json")
			inventoryFixture(t, path, tc.content)
			items, err := readProvidedInventory(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("error exposed input")
			}
			if tc.name == "valid" && (len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "beta") {
				t.Fatalf("unexpected order: %+v", items)
			}
		})
	}
}

func TestInventoryOptionalRootsAndExplicitFailure(t *testing.T) {
	home := inventoryTestHome(t)
	inv, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil || !inv.Complete {
		t.Fatalf("absent optional roots: %v %+v", err, inv)
	}
	inv, err = loadInventoryAtHome("unknown", "", "", home)
	if err != nil || inv.Complete || len(inv.Warnings) == 0 {
		t.Fatalf("unknown provider: %v %+v", err, inv)
	}
	inv, err = loadInventoryAtHome("codex", "", filepath.Join(home, "missing-private-name"), home)
	if err == nil || inv.Complete || strings.Contains(err.Error(), home) {
		t.Fatalf("explicit input failure: %v %+v", err, inv)
	}
}

func TestInventoryEntryLimit(t *testing.T) {
	home := inventoryTestHome(t)
	root := filepath.Join(home, "selected-codex", "skills")
	for i := 0; i < inventoryEntryLimit+1; i++ {
		inventoryFixture(t, filepath.Join(root, string(rune(0x1000+i))), "ignored")
	}
	inv, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil || inv.Complete {
		t.Fatalf("entry bound: %v %+v", err, inv)
	}
}

func TestOverlapHintsGenericLexicalRanking(t *testing.T) {
	inv := Inventory{Items: []InventoryItem{
		{ID: "a", Name: "alpha", Description: "records records"},
		{ID: "b", Name: "beta", Description: "correlate records"},
		{ID: "c", Name: "gamma", Description: "correlate records"},
		{ID: "d", Name: "delta", Description: "unrelated"},
	}}
	hints := OverlapHints([]string{"CORRELATE", "records", "records"}, inv, 2)
	if len(hints) != 2 || hints[0].ID != "b" || hints[1].ID != "c" {
		t.Fatalf("unexpected ranking: %+v", hints)
	}
	if len(OverlapHints([]string{"absent"}, inv, 5)) != 0 || len(OverlapHints([]string{"records"}, inv, 0)) != 0 {
		t.Fatal("unexpected empty match")
	}
}

func TestInventoryExplicitMetadataRedactedAndDeduplicated(t *testing.T) {
	home := inventoryTestHome(t)
	extra := filepath.Join(home, "provided.json")
	inventoryFixture(t, extra, `[{"name":"user@example.com","description":"Use sk-123456789012345678901234567890 at /home/private/project"},{"name":"user@example.com","description":"Use sk-123456789012345678901234567890 at /home/private/project"}]`)
	inv, err := loadInventoryAtHome("codex", "", extra, home)
	if err != nil || !inv.Complete {
		t.Fatalf("load: %v %+v", err, inv)
	}
	var provided []InventoryItem
	for _, item := range inv.Items {
		if item.Source == "provided_inventory" {
			provided = append(provided, item)
		}
	}
	if len(provided) != 1 {
		t.Fatalf("duplicate provided items: %+v", provided)
	}
	data, _ := json.Marshal(provided)
	for _, secret := range []string{"user@example.com", "sk-123456789012345678901234567890", "/home/private/project"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("unredacted metadata: %s", data)
		}
	}
}

func TestInventoryBrokenSkillSymlinkIsIncomplete(t *testing.T) {
	home := inventoryTestHome(t)
	root := filepath.Join(home, "selected-codex", "skills")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	inv, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil || inv.Complete {
		t.Fatalf("broken symlink: %v %+v", err, inv)
	}
}

func TestInventoryRejectsProjectSkillEscapes(t *testing.T) {
	for _, kind := range []string{"directory", "metadata", "root"} {
		t.Run(kind, func(t *testing.T) {
			home := inventoryTestHome(t)
			project := filepath.Join(home, "project")
			root := filepath.Join(project, ".agents", "skills")
			outside := filepath.Join(home, "private")
			inventoryFixture(t, filepath.Join(outside, "SKILL.md"), "---\nname: ESCAPE_PRIVATE_MARKER\ndescription: private outside metadata\n---\n")
			if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "root":
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				skill := filepath.Join(root, "escape")
				if err := os.MkdirAll(skill, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(skill, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			inv, err := loadInventoryAtHome("codex", project, "", home)
			if err != nil || inv.Complete || inv.SkillsSeen != 0 {
				t.Fatalf("escape status: %v %+v", err, inv)
			}
			data, _ := json.Marshal(inv)
			if strings.Contains(string(data), "ESCAPE_PRIVATE_MARKER") || strings.Contains(string(data), outside) {
				t.Fatalf("escape data leaked: %s", data)
			}
		})
	}
}

func TestInventoryRejectsGlobalMetadataFileEscape(t *testing.T) {
	home := inventoryTestHome(t)
	root := filepath.Join(home, "selected-codex", "skills", "unsafe")
	outside := filepath.Join(home, "private", "record")
	inventoryFixture(t, outside, "---\nname: ESCAPE_PRIVATE_MARKER\ndescription: private record\n---\n")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	inv, err := loadInventoryAtHome("codex", "", "", home)
	if err != nil || inv.Complete || inv.SkillsSeen != 0 {
		t.Fatalf("escape status: %v %+v", err, inv)
	}
	data, _ := json.Marshal(inv)
	if strings.Contains(string(data), "ESCAPE_PRIVATE_MARKER") {
		t.Fatalf("escape data leaked: %s", data)
	}
}

func TestInventoryAllowsProjectLinksWithinSkillRoot(t *testing.T) {
	home := inventoryTestHome(t)
	project := filepath.Join(home, "project")
	root := filepath.Join(project, ".agents", "skills")
	target := filepath.Join(root, "actual")
	inventoryFixture(t, filepath.Join(target, "SKILL.md"), "---\nname: local\ndescription: local metadata\n---\n")
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	inv, err := loadInventoryAtHome("codex", project, "", home)
	if err != nil || !inv.Complete || inv.SkillsSeen != 1 {
		t.Fatalf("contained link: %v %+v", err, inv)
	}
}

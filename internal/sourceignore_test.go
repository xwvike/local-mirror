package app

import (
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// 源端忽略的路径不在源端树里是因为被忽略：汇端不得把它当作上游已删除。
// 只拦删除——源端树里真有的条目（新建/修改）照常同步
func TestFilterIgnoredDiffsKeepsSourceIgnoredPaths(t *testing.T) {
	saved := config.IgnoreFileList
	defer func() { config.IgnoreFileList = saved }()
	config.IgnoreFileList = []string{".local-mirror", "*.tmp"}

	diffs := []DiffResult{
		{Path: ".git", Action: "delete", IsDir: true},
		{Path: filepath.Join("web", "node_modules"), Action: "delete", IsDir: true},
		{Path: "old.txt", Action: "delete"},
		{Path: "x.tmp", Action: "delete"},
		{Path: filepath.Join("web", "node_modules", "pkg.json"), Action: "create"},
	}
	kept := filterIgnoredDiffs(diffs, []string{".git", "node_modules"})
	var got []string
	for _, d := range kept {
		got = append(got, d.Action+" "+d.Path)
	}
	want := []string{"delete old.txt", "create " + filepath.Join("web", "node_modules", "pkg.json")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("kept %v, want %v", got, want)
	}
}

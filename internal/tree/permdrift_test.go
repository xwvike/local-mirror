package tree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"local-mirror/config"
)

// 纯汇端重建树时，本地被改过的权限按记录的上游值改回；源端（含中继）则以磁盘为准
func TestRebuildRestoresLocallyChangedPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions on windows")
	}
	saveMode := config.Mode
	defer func() { config.Mode = saveMode }()

	for _, mode := range []string{"mirror", "reality"} {
		t.Run(mode, func(t *testing.T) {
			m := mode
			config.Mode = &m
			root := t.TempDir()
			config.StartPath = root
			config.IgnoreFileList = []string{".local-mirror"}
			f := filepath.Join(root, "secret.txt")
			if err := os.WriteFile(f, []byte("k"), 0o600); err != nil {
				t.Fatal(err)
			}

			InitDB()
			defer DB.Close()
			if err := BuildFileTree(root); err != nil {
				t.Fatal(err)
			}
			// 模拟汇端已应用上游权限 0600 后，本地被人放宽成 0644
			if err := os.Chmod(f, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := BuildFileTree(root); err != nil {
				t.Fatal(err)
			}

			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			node, err := GetNodeByPath("secret.txt")
			if err != nil {
				t.Fatal(err)
			}
			wantDisk, wantNode := os.FileMode(0o600), uint32(0o600)
			if mode == "reality" {
				wantDisk, wantNode = 0o644, 0o644
			}
			if fi.Mode().Perm() != wantDisk {
				t.Errorf("disk mode %o, want %o", fi.Mode().Perm(), wantDisk)
			}
			if node.Mode != wantNode {
				t.Errorf("node mode %o, want %o", node.Mode, wantNode)
			}
		})
	}
}

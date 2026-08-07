package app

import (
	"testing"

	"local-mirror/internal/tree"
)

// TestFindDifferencesTypeSwap 验证 COR-03：同一路径的文件↔目录类型互换必须被检出为
// 独立的 retype 动作，而不是当成 modify 或（大小碰巧相同时）被完全漏掉。
func TestFindDifferencesTypeSwap(t *testing.T) {
	// server: x 是文件；local: x 是目录 → retype，新类型为文件
	a := []tree.Node{{Path: "x", IsDir: false, Size: 10, Hash: "h1"}}
	b := []tree.Node{{Path: "x", IsDir: true, Size: 10, Hash: ""}}
	if d := FindDifferences(a, b); len(d) != 1 || d[0].Action != "retype" || d[0].IsDir {
		t.Fatalf("file<-dir 应产出 retype(新类型=文件)，实际 %+v", d)
	}

	// 反向：server 目录，local 文件 → retype，新类型为目录
	a2 := []tree.Node{{Path: "x", IsDir: true, Size: 0, Hash: ""}}
	b2 := []tree.Node{{Path: "x", IsDir: false, Size: 0, Hash: "h2"}}
	if d := FindDifferences(a2, b2); len(d) != 1 || d[0].Action != "retype" || !d[0].IsDir {
		t.Fatalf("dir<-file 应产出 retype(新类型=目录)，实际 %+v", d)
	}

	// 类型不同但大小相同、哈希不可比：仍必须检出 retype（旧实现在此完全漏掉）
	a3 := []tree.Node{{Path: "x", IsDir: false, Size: 4096, Hash: ""}}
	b3 := []tree.Node{{Path: "x", IsDir: true, Size: 4096, Hash: ""}}
	if d := FindDifferences(a3, b3); len(d) != 1 || d[0].Action != "retype" {
		t.Fatalf("大小相同的类型互换仍应检出 retype，实际 %+v", d)
	}

	// 控制：同类型、同大小、同哈希 → 无 diff
	a4 := []tree.Node{{Path: "x", IsDir: false, Size: 10, Hash: "h"}}
	b4 := []tree.Node{{Path: "x", IsDir: false, Size: 10, Hash: "h"}}
	if d := FindDifferences(a4, b4); len(d) != 0 {
		t.Fatalf("同类型同内容不该产出 diff，实际 %+v", d)
	}
}

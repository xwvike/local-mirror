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

// TestFindDifferencesDirSizeIgnored 守住 Merkle 剪枝的前提：同名目录即便 Size 不同（跨文件
// 系统天然如此，源 APFS vs 汇 ext4），也不得产出 modify——否则每个目录每轮全量扫描都被标记
// 变更，走 diff 循环的下钻 push 绕过 rollup 剪枝，剪枝失效、流量照烧。
func TestFindDifferencesDirSizeIgnored(t *testing.T) {
	// 同路径、都是目录、Size 不同（4096 vs 64，模拟不同文件系统的目录项大小）
	a := []tree.Node{{Path: "d", IsDir: true, Size: 4096}}
	b := []tree.Node{{Path: "d", IsDir: true, Size: 64}}
	if d := FindDifferences(a, b); len(d) != 0 {
		t.Fatalf("同名目录 Size 不同不应产出 diff（目录无 modify），实际 %+v", d)
	}

	// 对照：文件 Size 不同仍必须是 modify（Option B 只豁免目录，不动文件）
	af := []tree.Node{{Path: "f", IsDir: false, Size: 20, Hash: "h2"}}
	bf := []tree.Node{{Path: "f", IsDir: false, Size: 10, Hash: "h1"}}
	if d := FindDifferences(af, bf); len(d) != 1 || d[0].Action != "modify" || d[0].IsDir {
		t.Fatalf("文件 Size 变化仍应产出 modify，实际 %+v", d)
	}
}

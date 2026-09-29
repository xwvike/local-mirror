package tree

import (
	"testing"
	"time"
)

// nodesByPath 从一组节点建 path→*Node 映射（computeDirHashes 的入参形态）
func nodesByPath(ns []*Node) map[string]*Node {
	m := make(map[string]*Node, len(ns))
	for _, n := range ns {
		m[n.Path] = n
	}
	return m
}

// sampleTree 一棵小树：. / a(dir) / a/f1(file) / a/b(dir) / a/b/f2(file)
func sampleTree() []*Node {
	t0 := time.Unix(1000, 0)
	return []*Node{
		{Path: ".", Name: ".", IsDir: true},
		{Path: "a", Name: "a", IsDir: true},
		{Path: "a/f1", Name: "f1", Size: 10, Hash: "aaaa", ModTime: t0},
		{Path: "a/b", Name: "b", IsDir: true},
		{Path: "a/b/f2", Name: "f2", Size: 20, Hash: "bbbb", ModTime: t0},
	}
}

func TestDirHashesStableAndDeterministic(t *testing.T) {
	h1 := computeDirHashes(nodesByPath(sampleTree()))
	h2 := computeDirHashes(nodesByPath(sampleTree()))

	for _, dir := range []string{".", "a", "a/b"} {
		if h1[dir] == "" {
			t.Fatalf("dir %q got empty rollup", dir)
		}
		if h1[dir] != h2[dir] {
			t.Fatalf("dir %q non-deterministic: %s vs %s", dir, h1[dir], h2[dir])
		}
	}
	// 不同层级的 rollup 不应雷同
	if h1["."] == h1["a"] || h1["a"] == h1["a/b"] {
		t.Fatalf("distinct dirs share a rollup: root=%s a=%s a/b=%s", h1["."], h1["a"], h1["a/b"])
	}
}

// mtime-only 变化不得改变 rollup（与 FindDifferences 口径一致：diff 不看 mtime）
func TestDirHashesIgnoresModTime(t *testing.T) {
	base := computeDirHashes(nodesByPath(sampleTree()))

	tree2 := sampleTree()
	for _, n := range tree2 {
		n.ModTime = n.ModTime.Add(48 * time.Hour) // 只动 mtime
	}
	after := computeDirHashes(nodesByPath(tree2))

	for _, dir := range []string{".", "a", "a/b"} {
		if base[dir] != after[dir] {
			t.Fatalf("mtime-only change altered rollup of %q: %s -> %s", dir, base[dir], after[dir])
		}
	}
}

// 文件内容（hash）变化必须改变其所在目录及所有祖先目录的 rollup，且只改这些
func TestDirHashesPropagateContentChange(t *testing.T) {
	base := computeDirHashes(nodesByPath(sampleTree()))

	tree2 := sampleTree()
	for _, n := range tree2 {
		if n.Path == "a/b/f2" {
			n.Hash = "cccc" // 深层文件内容变了
		}
	}
	after := computeDirHashes(nodesByPath(tree2))

	// 祖先链 a/b、a、. 都必须变
	for _, dir := range []string{"a/b", "a", "."} {
		if base[dir] == after[dir] {
			t.Fatalf("content change did not propagate to ancestor %q", dir)
		}
	}
}

// 文件大小变化同样传播（size 是 diff 判定字段）
func TestDirHashesPropagateSizeChange(t *testing.T) {
	base := computeDirHashes(nodesByPath(sampleTree()))
	tree2 := sampleTree()
	for _, n := range tree2 {
		if n.Path == "a/f1" {
			n.Size = 999
		}
	}
	after := computeDirHashes(nodesByPath(tree2))
	if base["a"] == after["a"] {
		t.Fatal("size change did not alter parent rollup")
	}
	if base["a/b"] != after["a/b"] {
		t.Fatal("size change in a/f1 wrongly altered sibling subtree a/b")
	}
}

// 空目录也应产生稳定 rollup，且两个空目录的 rollup 相等（内容都为空）
func TestDirHashesEmptyDir(t *testing.T) {
	ns := []*Node{
		{Path: ".", Name: ".", IsDir: true},
		{Path: "empty1", Name: "empty1", IsDir: true},
		{Path: "empty2", Name: "empty2", IsDir: true},
	}
	h := computeDirHashes(nodesByPath(ns))
	if h["empty1"] == "" || h["empty2"] == "" {
		t.Fatal("empty dir got no rollup")
	}
	if h["empty1"] != h["empty2"] {
		t.Fatal("two empty dirs should share the same rollup")
	}
}

// v2.5.0 对 sampleTree 算出的 rollup（由 v2.5.0 源码独立生成）。线格式 Node.Hash 的口径
// 必须与它逐字节一致，否则新版源端配旧版汇端时剪枝全部失效
var legacyGolden = map[string]string{
	".":   "4cd7d92fa1466fe0890f7513e852a1c2f400811c46f4fe8413570f279deed777",
	"a":   "bd73b92f8b2c0213c24b12e3453f54b9225fe0ef0fd3b12ca6f1f8be0e9c78c2",
	"a/b": "d9e079e1a7e2235d880530ac2db2ec0870915f4e3618de3f0499b4cec2f471fd",
}

func withModes(ns []*Node) []*Node {
	for _, n := range ns {
		if n.IsDir {
			n.Mode = 0o755
		} else {
			n.Mode = 0o600
		}
	}
	return ns
}

// 不含权限的口径与 v2.5.0 逐字节一致，且节点带不带权限都不影响它
func TestDirHashesMatchV250(t *testing.T) {
	for name, ns := range map[string][]*Node{"no modes": sampleTree(), "with modes": withModes(sampleTree())} {
		h := computeDirHashes(nodesByPath(ns))
		for dir, want := range legacyGolden {
			if h[dir] != want {
				t.Errorf("%s: legacy rollup of %q = %s, v2.5.0 had %s", name, dir, h[dir], want)
			}
		}
	}
}

// 权限未知（0）时含权限口径与不含权限口径相同：Windows/旧版对端的节点不因口径分叉
func TestPermRollupUnknownModeEqualsLegacy(t *testing.T) {
	nodes := nodesByPath(sampleTree())
	plain, perm := computeRollups(nodes, false), computeRollups(nodes, true)
	for dir := range legacyGolden {
		if plain[dir] != perm[dir] {
			t.Errorf("dir %q: perm rollup %s differs from legacy %s with all modes unknown", dir, perm[dir], plain[dir])
		}
	}
}

// 仅权限变化：含权限口径沿祖先链传播，不含权限口径不动
func TestPermRollupPropagatesModeChange(t *testing.T) {
	base := withModes(sampleTree())
	changed := withModes(sampleTree())
	for _, n := range changed {
		if n.Path == "a/b/f2" {
			n.Mode = 0o644
		}
	}
	permBase, permAfter := computeRollups(nodesByPath(base), true), computeRollups(nodesByPath(changed), true)
	plainBase, plainAfter := computeRollups(nodesByPath(base), false), computeRollups(nodesByPath(changed), false)
	for _, dir := range []string{"a/b", "a", "."} {
		if permBase[dir] == permAfter[dir] {
			t.Errorf("mode change did not propagate to perm rollup of %q", dir)
		}
		if plainBase[dir] != plainAfter[dir] {
			t.Errorf("mode change altered legacy rollup of %q", dir)
		}
	}
}

// 目录按 EffectiveMode 计入：只差属主位（0555 vs 0755）的目录在汇端落地后相同，rollup 也须相同
func TestPermRollupDirOwnerBitsIgnored(t *testing.T) {
	src, dst := withModes(sampleTree()), withModes(sampleTree())
	for _, n := range src {
		if n.Path == "a/b" {
			n.Mode = 0o555
		}
	}
	a, b := computeRollups(nodesByPath(src), true), computeRollups(nodesByPath(dst), true)
	for _, dir := range []string{"a", "."} {
		if a[dir] != b[dir] {
			t.Errorf("owner-bit-only dir mode difference changed perm rollup of %q", dir)
		}
	}
}

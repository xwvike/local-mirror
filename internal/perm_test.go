package app

import (
	"testing"

	"local-mirror/internal/tree"
)

// rollupMatches 按对端给的口径比：有 PermRollup 只比含权限快照（不回落到 Hash），
// 没有才比不含权限快照；任一侧缺失都不剪枝
func TestRollupMatches(t *testing.T) {
	saveP, saveM := localDirHashes, localPermDirHashes
	defer func() { localDirHashes, localPermDirHashes = saveP, saveM }()
	localDirHashes = map[string]string{"d": "plain"}
	localPermDirHashes = map[string]string{"d": "perm"}

	cases := []struct {
		name string
		node tree.Node
		want bool
	}{
		{"perm rollup matches", tree.Node{Path: "d", Hash: "x", PermRollup: "perm"}, true},
		{"perm rollup differs, legacy would match", tree.Node{Path: "d", Hash: "plain", PermRollup: "other"}, false},
		{"old source: legacy matches", tree.Node{Path: "d", Hash: "plain"}, true},
		{"old source: legacy differs", tree.Node{Path: "d", Hash: "other"}, false},
		{"no rollup from peer", tree.Node{Path: "d"}, false},
		{"dir missing locally", tree.Node{Path: "new", Hash: "plain", PermRollup: "perm"}, false},
	}
	for _, c := range cases {
		if got := rollupMatches(c.node); got != c.want {
			t.Errorf("%s: rollupMatches = %v, want %v", c.name, got, c.want)
		}
	}

	localDirHashes, localPermDirHashes = nil, nil
	if rollupMatches(tree.Node{Path: "d", Hash: "plain", PermRollup: "perm"}) {
		t.Error("pruned without a local snapshot")
	}
}

// chmod 判定与含权限 rollup 同口径：rollup 相等的子树不得藏着 chmod 差异，反之亦然
func TestFindDifferencesChmod(t *testing.T) {
	const h = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	file := func(mode uint32, hash string) tree.Node {
		return tree.Node{Path: "f", Name: "f", Size: 3, Hash: hash, Mode: mode}
	}
	dir := func(mode uint32) tree.Node { return tree.Node{Path: "d", Name: "d", IsDir: true, Mode: mode} }

	cases := []struct {
		name   string
		a, b   tree.Node
		action string // "" = 无差异
	}{
		{"file mode differs", file(0o600, h), file(0o644, h), "chmod"},
		{"legacy sink node (mode unknown)", file(0o600, h), file(0, h), "chmod"},
		{"upstream mode unknown", file(0, h), file(0o644, h), ""},
		{"upstream unreadable", file(0o000, ""), file(0o644, h), ""},
		{"content and mode differ", file(0o600, h), tree.Node{Path: "f", Name: "f", Size: 4, Hash: h, Mode: 0o644}, "modify"},
		{"dir owner bits only", dir(0o555), dir(0o755), ""},
		{"dir group/other differ", dir(0o700), dir(0o755), "chmod"},
	}
	for _, c := range cases {
		d := FindDifferences([]tree.Node{c.a}, []tree.Node{c.b})
		got := ""
		if len(d) == 1 {
			got = d[0].Action
		} else if len(d) > 1 {
			t.Errorf("%s: %d diffs, want at most 1", c.name, len(d))
			continue
		}
		if got != c.action {
			t.Errorf("%s: action %q, want %q", c.name, got, c.action)
		}
	}
}

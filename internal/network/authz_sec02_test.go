package network

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
	"local-mirror/internal/tree"
)

// TestAuthorizeServeFileSEC02 验证文件服务的授权闸门（SEC-02）：只有「公开目录树里、
// 哈希非空的普通文件」才允许下发；.local-mirror 状态目录、忽略项、目录节点、不在树的
// 路径一律拒绝。这堵住「已握手对端绕过树枚举直接点名任意路径」的越权读取。
func TestAuthorizeServeFileSEC02(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	// 故意不把 .local-mirror 放进忽略列表（模拟 !.local-mirror 取消，或忽略列表被改坏），
	// 以此证明 .local-mirror 的硬拒 ① 不依赖可配置忽略列表；另放一条自定义模式 *.key 测 ②。
	config.IgnoreFileList = []string{"*.key"}

	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("foo.txt", "hello")
	mustWrite(filepath.Join("sub", "bar.txt"), "world")
	mustWrite("secret.key", "TOPSECRET") // 命中 *.key → 忽略

	tree.InitDB() // 建 <root>/.local-mirror/cache.db（本测忽略列表不含它，故会进树）
	defer tree.DB.Close()
	if err := tree.BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	allowed := func(rel string) bool { return authorizeServeFile(rel, rel) == nil }

	// 正常普通文件：放行
	if !allowed("foo.txt") {
		t.Error("foo.txt（树中的普通文件）应被允许下发")
	}
	if !allowed(filepath.Join("sub", "bar.txt")) {
		t.Error("sub/bar.txt 应被允许下发")
	}

	// 目录节点：拒绝（node.IsDir）
	if allowed("sub") {
		t.Error("目录 sub 不该作为文件下发")
	}

	// ① .local-mirror 独立硬拒——即便它在树里（本测忽略列表不含它、且有非空哈希）也必须拒。
	// 这是关键：证明状态目录的保护不依赖可配置忽略列表。
	lmCache := filepath.Join(".local-mirror", "cache.db")
	if node, err := tree.GetNodeByPath(lmCache); err != nil || node == nil || node.Hash == "" {
		t.Fatalf("前置失败：本测应让 .local-mirror/cache.db 带哈希进树，node=%v err=%v", node, err)
	}
	if allowed(lmCache) {
		t.Error(".local-mirror/cache.db 必须被硬拒，即使它在树中且有哈希")
	}

	// ② 命中忽略列表：拒绝
	if allowed("secret.key") {
		t.Error("命中 *.key 的 secret.key 不该被下发")
	}

	// ③ 不在树中的路径：拒绝（探测 .git/config、任意猜测路径都走这条）
	if allowed("nope.txt") {
		t.Error("不在树中的 nope.txt 应被拒绝")
	}
	if allowed(filepath.Join(".git", "config")) {
		t.Error(".git/config（不在树中）应被拒绝")
	}
}

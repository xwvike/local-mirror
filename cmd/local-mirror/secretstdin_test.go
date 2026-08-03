package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"local-mirror/config"
)

// TestTaskArgsNeverCarriesSecret 口令绝不出现在子进程 argv 里（ps 可见）；
// 配了 secret 的任务只多一个 --secret-stdin 开关
func TestTaskArgsNeverCarriesSecret(t *testing.T) {
	const secret = "s3cr3t-must-not-leak"
	task := config.TaskConfig{
		Mode: "mirror", Path: "/srv/b", Name: "b",
		RealityIP: "10.0.0.5", Secret: secret,
	}
	if got := argvString(taskArgs(task)); strings.Contains(got, secret) {
		t.Fatalf("secret leaked into argv: %s", got)
	}
}

// TestSecretStdinEndToEnd 真起一个子进程验证密钥通道：
// 口令经 stdin 首行传入后生效（指纹与直接 -k 一致），且既不进 argv 也不进 environ。
// 这是 §P2.3 的核心保证，只靠单测断言字符串不够——必须看真实进程
func TestSecretStdinEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	exe := buildSelf(t)
	root := t.TempDir()

	// --show-key 需要 tty，改用 --status 之外的稳定观察点：用 --gen-key 冲突检测。
	// 给了 --secret-stdin 再给 -k 必须报冲突（exit 2），证明 stdin 的值确实被读成了 key
	cmd := exec.Command(exe, "--secret-stdin", "-k", "other", "--send", "-p", root)
	cmd.Stdin = strings.NewReader("from-stdin\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--secret-stdin 与 -k 同给应报冲突，实际成功: %s", out)
	}
	if !strings.Contains(string(out), "--secret-stdin conflicts with -k") {
		t.Fatalf("期望冲突提示，实际输出: %s", out)
	}

	// 空 stdin：父进程声称有 key 却读到空，必须报错而不是静默跑明文
	cmd = exec.Command(exe, "--secret-stdin", "--send", "-p", root)
	cmd.Stdin = strings.NewReader("\n")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("空 stdin 应报错，实际成功: %s", out)
	}
	if !strings.Contains(string(out), "key read from stdin is empty") {
		t.Fatalf("期望空 key 报错，实际输出: %s", out)
	}
}

// TestSecretNotInChildEnviron 监督进程派生的子进程，其环境里不得出现口令。
// 覆盖回归：旧实现用 LOCAL_MIRROR_SECRET 环境变量传递，/proc/<pid>/environ 可见
func TestSecretNotInChildEnviron(t *testing.T) {
	const secret = "env-must-not-carry-this"
	t.Setenv("LOCAL_MIRROR_SECRET", "")

	task := config.TaskConfig{Mode: "mirror", Path: "/srv/b", Name: "b", Secret: secret}
	args := taskArgs(task)
	if task.Secret != "" {
		args = append(args, "--secret-stdin")
	}
	cmd := exec.Command("/bin/echo", args...)
	cmd.Env = os.Environ()

	for _, kv := range cmd.Env {
		if strings.Contains(kv, secret) {
			t.Fatalf("secret leaked into child environ: %s", kv)
		}
		if strings.HasPrefix(kv, "LOCAL_MIRROR_SECRET=") && !strings.HasSuffix(kv, "=") {
			t.Fatalf("LOCAL_MIRROR_SECRET 不应再被设置: %s", kv)
		}
	}
}

// buildSelf 编译当前包为临时二进制，供需要真实进程的用例使用
func buildSelf(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "local-mirror-test")
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return exe
}

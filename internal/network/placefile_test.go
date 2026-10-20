package network

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// 目标在另一个挂载点时 rename 报跨设备：改走复制 + 目标目录内 rename，分片清掉、权限保留
func TestPlaceFileCrossDevice(t *testing.T) {
	saved := renameFile
	defer func() { renameFile = saved }()
	errno := syscall.EXDEV
	if runtime.GOOS == "windows" {
		errno = 17
	}
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errno}
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "x.part")
	dst := filepath.Join(dir, "sub", "x")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := placeFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Errorf("destination = %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("partial left behind")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(dst); info.Mode().Perm() != 0640 {
			t.Errorf("perm = %o, want 640", info.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "sub", ".x.lm-tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// 其他 rename 错误原样返回，不去复制
func TestPlaceFileOtherErrorsUntouched(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.part")
	if err := os.WriteFile(src, []byte("d"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "missing-dir", "x")
	if err := placeFile(src, dst); err == nil {
		t.Fatal("rename into a missing directory succeeded")
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("partial removed although nothing was placed")
	}
}

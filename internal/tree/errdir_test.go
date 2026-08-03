package tree

import (
	"errors"
	"testing"

	"local-mirror/config"
)

// TestGetDirContentsErrDirNotFound 树索引中不存在的路径必须返回可被
// errors.Is 命中的 ErrDirNotFound，供 watcher 剔除孤儿热度条目而不靠字符串匹配
func TestGetDirContentsErrDirNotFound(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	_, err := GetDirContents("does/not/exist")
	if err == nil {
		t.Fatal("缺失路径应报错")
	}
	if !errors.Is(err, ErrDirNotFound) {
		t.Fatalf("错误应可被 errors.Is(ErrDirNotFound) 命中, got: %v", err)
	}
}

package utils

import "testing"

func TestDiskFree(t *testing.T) {
	free, err := DiskFree(t.TempDir())
	if err != nil {
		t.Fatalf("DiskFree 对存在的目录不应报错: %v", err)
	}
	if free == 0 {
		t.Fatal("临时目录所在文件系统的可用空间不应为 0")
	}
}

func TestDiskFreeNonexistent(t *testing.T) {
	if _, err := DiskFree("/lm-definitely-nonexistent-path"); err == nil {
		t.Fatal("不存在的路径应报错")
	}
}

package app

import "testing"

// 上游哈希缺失（服务端读不了的文件）必须被确定性跳过：
// 不触网络（fileClient 传 nil，若走到下载会直接 panic）、不报错
func TestProcessDiffItemSkipsUnreadableUpstream(t *testing.T) {
	v := DiffResult{
		Path:   "secret.txt",
		Action: "create",
		IsDir:  false,
		Hash:   "", // 服务端扫描时哈希失败
		Size:   123,
	}
	if err := processDiffItem(v, nil); err != nil {
		t.Fatalf("空哈希文件应被跳过而非报错: %v", err)
	}
	// modify 同样跳过
	v.Action = "modify"
	if err := processDiffItem(v, nil); err != nil {
		t.Fatalf("空哈希文件的 modify 应被跳过而非报错: %v", err)
	}
}

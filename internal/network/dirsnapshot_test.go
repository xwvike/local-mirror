package network

import (
	"fmt"
	"path/filepath"
	"testing"

	"local-mirror/internal/tree"
)

// TestDirSnapshotPagingNoCacheCorruption 验证 PERF-01 的关键正确性点：分页遍历时对每页做
// 线格式**副本**，缓存快照（已排序切片）不能被污染——否则原地改会清空 ID、把 Path 改成 "/"
// 形式写回缓存，后续页的游标比较（基于 OS 分隔符路径）随之错乱。
func TestDirSnapshotPagingNoCacheCorruption(t *testing.T) {
	// 模拟 dirSnapshot：逆序放入后排序一次
	entries := make([]tree.Node, 0, 25)
	for i := 24; i >= 0; i-- {
		entries = append(entries, tree.Node{
			ID:   fmt.Sprintf("id%02d", i),
			Path: filepath.Join("dir", fmt.Sprintf("f%02d", i)),
		})
	}
	sortNodesByPath(entries)

	orig := make([]string, len(entries))
	for i := range entries {
		orig[i] = entries[i].Path
	}

	// 分页遍历，每页做线格式副本（模拟 handleTreeRequest 复用缓存快照的路径）
	var got []string
	cursor := ""
	pages := 0
	for {
		page, next := pageSortedEntries(entries, cursor, 10)
		pages++
		for _, n := range wirePageCopy(page) {
			got = append(got, n.Path) // 副本应为 "/" 形式
			if n.ID != "" {
				t.Error("线格式副本应清空 ID")
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 || len(got) != 25 {
		t.Fatalf("25 条 / 每页 10 应为 3 页共 25 条，实际 %d 页 %d 条", pages, len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("聚合结果未严格有序：%q >= %q", got[i-1], got[i])
		}
	}

	// 关键：缓存快照未被污染——路径仍是原始 OS 分隔符形式、ID 仍在
	for i := range entries {
		if entries[i].Path != orig[i] {
			t.Errorf("缓存快照被污染：entries[%d].Path=%q，原为 %q", i, entries[i].Path, orig[i])
		}
		if entries[i].ID == "" {
			t.Errorf("缓存快照 ID 在 index %d 被清空", i)
		}
	}
}

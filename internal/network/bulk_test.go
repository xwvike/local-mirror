package network

import (
	"encoding/binary"
	"local-mirror/config"
	"local-mirror/internal/tree"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestBulkMessagesRoundTrip(t *testing.T) {
	req := BulkRequestMessage{
		Ignore:       []string{".git", "*.tmp"},
		Cursor:       filepath.Join("a", "b", "c.txt"),
		ResumePath:   filepath.Join("m", "big.bin"),
		ResumeOffset: 12345,
	}
	gotReq, err := decodeBulkRequest(encodeBulkRequest(req))
	if err != nil || !reflect.DeepEqual(gotReq, req) {
		t.Errorf("request round trip: got %+v, %v", gotReq, err)
	}

	plan := BulkPlanResponseMessage{Files: 20000, Bytes: 1 << 40}
	if got, err := decodeBulkPlanResponse(encodeBulkPlanResponse(plan)); err != nil || got != plan {
		t.Errorf("plan round trip: got %+v, %v", got, err)
	}

	entry := BulkEntryMessage{Kind: BulkEntryFile, Path: filepath.Join("d", "f"), Mode: 0o640,
		ModTime: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano(), Size: 99, Offset: 7}
	entry.SessionID[0], entry.FileHash[31] = 1, 2
	if got, err := decodeBulkEntry(encodeBulkEntry(entry)); err != nil || got != entry {
		t.Errorf("entry round trip: got %+v, %v", got, err)
	}

	end := BulkEndMessage{Files: 3, Bytes: 4}
	if got, err := decodeBulkEnd(encodeBulkEnd(end)); err != nil || got != end {
		t.Errorf("end round trip: got %+v, %v", got, err)
	}
}

// 伪造的超大规则计数必须在分配前被拒绝
func TestDecodeBulkRequestRejectsForgedCount(t *testing.T) {
	body := binary.BigEndian.AppendUint16(nil, 60000)
	if _, err := decodeBulkRequest(body); err == nil {
		t.Error("a forged ignore-pattern count was accepted")
	}
}

// 推送顺序与续推游标的比较必须与遍历一致：逐级比较路径组件，祖先先于子孙。
// 与整串字节序不同："a/b" 在 "a-c" 之前（'-' 的字节值小于 '/'）
func TestCompareTreeOrder(t *testing.T) {
	j := filepath.Join
	ordered := []string{"a", j("a", "b"), j("a", "b", "c"), j("a", "c"), "a-c", "b"}
	for i := range ordered {
		for k := range ordered {
			got := compareTreeOrder(ordered[i], ordered[k])
			switch {
			case i < k && got >= 0, i > k && got <= 0, i == k && got != 0:
				t.Errorf("compareTreeOrder(%q, %q) = %d", ordered[i], ordered[k], got)
			}
		}
	}
}

// bulkWalk 决定推送什么、以什么顺序推：跳过两端的忽略项与读不了的文件，
// 按 compareTreeOrder 递增；带游标时只推游标之后的条目，但仍要进入包含游标的目录
func TestBulkWalk(t *testing.T) {
	config.StartPath = t.TempDir()
	saved := config.IgnoreFileList
	config.IgnoreFileList = []string{".local-mirror", "*.tmp"}
	defer func() { config.IgnoreFileList = saved }()
	tree.InitDB()
	defer tree.DB.Close()

	j := filepath.Join
	nodes := []*tree.Node{{ID: "root", Path: ".", IsDir: true}}
	add := func(path string, isDir bool, hash string) {
		nodes = append(nodes, &tree.Node{ID: path, Path: path, Name: filepath.Base(path), IsDir: isDir, Hash: hash, Size: 1})
	}
	add("z", true, "")
	add(j("z", "f"), false, "h")
	add("a", true, "")
	add(j("a", "b"), true, "")
	add(j("a", "b", "x"), false, "h")
	add(j("a", "b", "y"), false, "h")
	add(j("a", "keep"), false, "h")
	add(j("a", "junk.tmp"), false, "h") // 源端忽略
	add(j("a", "private"), true, "")    // 汇端忽略，整棵子树不推
	add(j("a", "private", "p"), false, "h")
	add(j("a", "unreadable"), false, "") // 哈希缺失：源端读不了
	add("a-c", false, "h")
	if err := tree.AddNodes(nodes); err != nil {
		t.Fatal(err)
	}

	walk := func(cursor string) []string {
		t.Helper()
		var got []string
		req := BulkRequestMessage{Ignore: []string{"private"}, Cursor: cursor}
		if err := bulkWalk(".", req, func(n tree.Node) error {
			got = append(got, n.Path)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	all := []string{"a", j("a", "b"), j("a", "b", "x"), j("a", "b", "y"), j("a", "keep"), "a-c", "z", j("z", "f")}
	if got := walk(""); !reflect.DeepEqual(got, all) {
		t.Errorf("full walk:\n got %v\nwant %v", got, all)
	}
	if !sort.SliceIsSorted(all, func(i, k int) bool { return compareTreeOrder(all[i], all[k]) < 0 }) {
		t.Error("walk order disagrees with compareTreeOrder")
	}
	// 游标位于 a/b/x：a、a/b 已推过不再推，但要进入它们找到 a/b/y
	want := []string{j("a", "b", "y"), j("a", "keep"), "a-c", "z", j("z", "f")}
	if got := walk(j("a", "b", "x")); !reflect.DeepEqual(got, want) {
		t.Errorf("walk after cursor:\n got %v\nwant %v", got, want)
	}
	// 游标正好是目录 a/b（目录已提交、内容尚未推送）：a/b 不再推，但它的内容排在它之后，必须推
	want = []string{j("a", "b", "x"), j("a", "b", "y"), j("a", "keep"), "a-c", "z", j("z", "f")}
	if got := walk(j("a", "b")); !reflect.DeepEqual(got, want) {
		t.Errorf("walk after a directory cursor:\n got %v\nwant %v", got, want)
	}
}

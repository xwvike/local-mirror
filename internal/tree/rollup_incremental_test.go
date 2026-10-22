package tree

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestIncrementalRollupsMatchFull 增量重算的结果必须与整树重算逐目录一致。
// 随机对真实树库做多轮增删改（加文件、改内容、改权限、删文件、建带内容的
// 子树、删整棵子树、同一路径目录换成文件），每轮之后比对 DirHashes/PermDirHashes
// 与从全部节点现算的结果
func TestIncrementalRollupsMatchFull(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	config.IgnoreFileList = []string{".local-mirror"}
	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	dirs := []string{"."}
	var files []string
	seq := 0
	newName := func(prefix string) string { seq++; return fmt.Sprintf("%s%d", prefix, seq) }
	join := func(dir, name string) string {
		if dir == "." {
			return name
		}
		return filepath.Join(dir, name)
	}
	file := func(path string) *Node {
		return &Node{ID: newName("id"), Path: path, Name: filepath.Base(path), Size: uint64(rng.IntN(1000)),
			Hash: fmt.Sprintf("%x", rng.Uint64()), Mode: 0o644, Depth: depthOf(path) - 1}
	}
	dir := func(path string) *Node {
		return &Node{ID: newName("id"), Path: path, Name: filepath.Base(path), IsDir: true, Mode: 0o755, Depth: depthOf(path) - 1}
	}
	remove := func(list []string, p string) []string {
		out := list[:0]
		for _, x := range list {
			if x != p && !isUnder(x, p) {
				out = append(out, x)
			}
		}
		return out
	}

	check := func(round int) {
		t.Helper()
		plain, err := DirHashes()
		if err != nil {
			t.Fatal(err)
		}
		perm, err := PermDirHashes()
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := LoadAllNodesByPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := computeRollups(nodes, false); !maps.Equal(plain, want) {
			t.Fatalf("round %d: incremental plain rollups differ from a full recompute (%d vs %d dirs)", round, len(plain), len(want))
		}
		if want := computeRollups(nodes, true); !maps.Equal(perm, want) {
			t.Fatalf("round %d: incremental perm rollups differ from a full recompute", round)
		}
	}

	check(0)
	dirHashMu.Lock()
	fullBefore := fullRollups
	dirHashMu.Unlock()
	for round := 1; round <= 300; round++ {
		var err error
		switch op := rng.IntN(7); {
		case op == 0 || len(files) == 0: // 新文件
			p := join(dirs[rng.IntN(len(dirs))], newName("f"))
			err = AddNodes([]*Node{file(p)})
			files = append(files, p)
		case op == 1: // 改内容
			err = AddNodes([]*Node{file(files[rng.IntN(len(files))])})
		case op == 2: // 改权限
			n := file(files[rng.IntN(len(files))])
			n.Mode = 0o600
			err = AddNodes([]*Node{n})
		case op == 3: // 删文件
			p := files[rng.IntN(len(files))]
			err = DeleteNodes([]string{p})
			files = remove(files, p)
		case op == 4: // 新子树：一个事务里建目录、子目录和文件
			d := join(dirs[rng.IntN(len(dirs))], newName("d"))
			sub := join(d, newName("s"))
			batch := []*Node{dir(d), dir(sub), file(join(d, newName("f"))), file(join(sub, newName("f")))}
			err = AddNodes(batch)
			dirs = append(dirs, d, sub)
			files = append(files, batch[2].Path, batch[3].Path)
		case op == 5 && len(dirs) > 1: // 删整棵子树
			d := dirs[1+rng.IntN(len(dirs)-1)]
			err = DeleteNodes([]string{d})
			dirs, files = remove(dirs, d), remove(files, d)
		case op == 6 && len(dirs) > 1: // 目录换成同名文件
			d := dirs[1+rng.IntN(len(dirs)-1)]
			if err = DeleteNodes([]string{d}); err == nil {
				err = AddNodes([]*Node{file(d)})
			}
			dirs, files = remove(dirs, d), append(remove(files, d), d)
		default:
			continue
		}
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// 有时连着改几轮才取一次，覆盖多笔变更合并重算
		if rng.IntN(3) > 0 {
			check(round)
		}
	}
	check(-1)
	dirHashMu.Lock()
	full := fullRollups - fullBefore
	dirHashMu.Unlock()
	if full != 0 {
		t.Errorf("%d full recomputes during the random rounds, every round should have been incremental", full)
	}
}

func isUnder(p, dir string) bool {
	return len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == filepath.Separator
}

// TestRollupsOverflowFallsBackToFull 变更多到超过增量上限时整树重算，结果照样正确
func TestRollupsOverflowFallsBackToFull(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	config.IgnoreFileList = []string{".local-mirror"}
	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := DirHashes(); err != nil {
		t.Fatal(err)
	}
	batch := make([]*Node, 0, rollupIncrementalMax+10)
	for i := range cap(batch) {
		p := fmt.Sprintf("f%05d", i)
		batch = append(batch, &Node{ID: "id" + p, Path: p, Name: p, Size: uint64(i), Hash: p})
	}
	if err := AddNodes(batch); err != nil {
		t.Fatal(err)
	}
	dirtyMu.Lock()
	all := rollupDirtyAll
	dirtyMu.Unlock()
	if !all {
		t.Error("exceeding rollupIncrementalMax should mark the cache for a full recompute")
	}
	plain, err := DirHashes()
	if err != nil {
		t.Fatal(err)
	}
	nodes, _ := LoadAllNodesByPath()
	if !maps.Equal(plain, computeRollups(nodes, false)) {
		t.Error("full recompute after overflow is wrong")
	}
}

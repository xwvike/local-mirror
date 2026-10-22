package tree

import (
	"encoding/hex"
	"errors"
	"maps"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zeebo/blake3"
	bolt "go.etcd.io/bbolt"
)

// 目录级 Merkle rollup 哈希：每个目录一个"子树内容指纹"，源/汇两端用同一套算法从各自的
// 树库算出。全量扫描据此剪枝——某子目录源、汇 rollup 相等即整棵子树无任何 diff，可以整棵跳过，
// 不必逐目录把清单拉回来比对。这把"静默期成本 ∝ 树大小"改成了"成本 ∝ 真实变化"，是
// idle-traffic 缺陷的根治（见 docs / sync-idle-traffic-cost）。
//
// rollup 必须与 FindDifferences 的判定口径完全一致，否则会误剪（漏同步）或空转（白走一遍）：
//   - 文件按 (Name, Size, Hash) 入哈希——正是 diff 判定 create/modify/delete 用的字段；
//   - **mtime 不入哈希**：FindDifferences 不看 mtime，纯 mtime 变动不产生 diff，
//     若纳入会让整棵子树被误判为"变了"而全量重走（安全但白费流量）；
//   - 只用 child.Name（basename，分隔符无关）而非完整路径——哈希要跨 mac/debian/windows
//     一致，不能掺入各端不同的路径分隔符。
//
// 两种口径并存：DirHashes 不含权限（v2.5.0 起的线格式 Node.Hash，旧版对端只认它）；
// PermDirHashes 另按 EffectiveMode 计入权限（0 = 未知不写入），与 FindDifferences 的
// chmod 判定同口径，经 Node.PermRollup 下发。汇端按对端给了哪种比哪种，新旧版本任意
// 组合、任意升级顺序剪枝都照常生效。
//
// 不变量：源目录 rollup == 汇同名目录 rollup  ⟺  该子树内 FindDifferences 无任何差异
// （不含权限的口径下不计 chmod 差异）。

var (
	// dirHashMu 串行化 rollup 的计算并保护两份缓存。缓存一经发布即只读：
	// 调用方（全量扫描）会在整轮扫描期间持有返回的 map，增量更新总是先复制再改
	dirHashMu        sync.Mutex
	dirHashCache     map[string]string // path → 该目录子树的 rollup 哈希（十六进制，不含权限）
	permDirHashCache map[string]string // 同上，含权限
	fullRollups      int               // 整树重算的次数（测试据此确认走的是增量）

	// dirtyMu 保护 rollupDirty/rollupDirtyAll。与 dirHashMu 分开：写事务提交后
	// 登记变更不必等一次正在进行的计算
	dirtyMu sync.Mutex
	// rollupDirty 自上次计算以来增删改过的节点路径
	rollupDirty map[string]struct{}
	// rollupDirtyAll 缓存整体作废（尚未算过、换了库、或变更多到不如全量重算）
	rollupDirtyAll = true
)

// rollupIncrementalMax 待重算路径超过此数就整树重算：全量是一次顺序扫描，
// 逐路径回查的总代价在这个量级上已不占优
const rollupIncrementalMax = 8192

// txChanged 当前写事务里增删改过的节点路径，由 update 设置。bbolt 同一时刻
// 只有一个写事务，且只在写事务内部读写，无需另加锁
var txChanged *[]string

// recordNodeChange 在写事务里登记一个被增删改的节点路径
func recordNodeChange(path string) {
	if txChanged != nil {
		*txChanged = append(*txChanged, path)
	}
}

// markRollupsDirty 登记一次已提交写事务改过的节点路径
func markRollupsDirty(paths []string) {
	if len(paths) == 0 {
		return
	}
	dirtyMu.Lock()
	defer dirtyMu.Unlock()
	if rollupDirtyAll {
		return
	}
	if rollupDirty == nil {
		rollupDirty = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		rollupDirty[p] = struct{}{}
	}
	if len(rollupDirty) > rollupIncrementalMax {
		rollupDirtyAll, rollupDirty = true, nil
	}
}

// invalidateRollups 作废全部 rollup 缓存，下次整树重算
func invalidateRollups() {
	dirtyMu.Lock()
	rollupDirtyAll, rollupDirty = true, nil
	dirtyMu.Unlock()
}

// DirHashes 返回每个目录路径到其子树 rollup 哈希（不含权限）的映射（键为本地分隔符路径，
// 根为 "."）。树自上次计算以来没变则直接返回缓存；变了只重算变更路径的祖先链，
// 代价随变化量而不是树的大小增长。并发安全（多客户端 goroutine 可同时调用）。
// 返回的 map 只读、不可修改。
func DirHashes() (map[string]string, error) {
	plain, _, err := dirRollups()
	return plain, err
}

// PermDirHashes 同 DirHashes，但 rollup 计入权限位
func PermDirHashes() (map[string]string, error) {
	_, perm, err := dirRollups()
	return perm, err
}

// dirRollups 返回的结果反映调用前已完成（update 已返回）的全部写入：写方提交
// 之后才登记变更，这里先取走登记、再开读事务，取走的每一项在读事务里都已可见。
// 取走之后才提交或登记的写入留给下一次调用
func dirRollups() (plain, perm map[string]string, err error) {
	dirHashMu.Lock()
	defer dirHashMu.Unlock()

	dirtyMu.Lock()
	all, changed := rollupDirtyAll, rollupDirty
	rollupDirtyAll, rollupDirty = false, nil
	dirtyMu.Unlock()

	if !all && len(changed) == 0 && dirHashCache != nil {
		return dirHashCache, permDirHashCache, nil
	}
	if !all && dirHashCache != nil {
		plain, perm, ok, err := incrementalRollups(dirHashCache, permDirHashCache, changed)
		if err == nil && ok {
			dirHashCache, permDirHashCache = plain, perm
			return plain, perm, nil
		}
		if err != nil {
			restoreDirty(false, changed)
			return nil, nil, err
		}
		// 增量所需的子目录 rollup 缺失（不该发生），整树重算兜底
	}

	nodes, err := LoadAllNodesByPath()
	if err != nil {
		restoreDirty(true, nil)
		return nil, nil, err
	}
	fullRollups++
	dirHashCache = computeRollups(nodes, false)
	permDirHashCache = computeRollups(nodes, true)
	return dirHashCache, permDirHashCache, nil
}

// restoreDirty 计算失败时把取走的登记放回，不能让这些变更从此不再重算
func restoreDirty(all bool, changed map[string]struct{}) {
	if all {
		invalidateRollups()
		return
	}
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	markRollupsDirty(paths)
}

// incrementalRollups 在 plain/perm 的副本上重算 changed 各路径的祖先链（含其自身，
// 若它是目录）。一个目录的 rollup 只取决于它的直接子项与子目录的 rollup，所以
// 由深到浅重算这些目录就够了，其余目录的值原样沿用。ok=false 表示缓存与树对不上
// （某个子目录没有 rollup），调用方应整树重算
func incrementalRollups(oldPlain, oldPerm map[string]string, changed map[string]struct{}) (plain, perm map[string]string, ok bool, err error) {
	plain, perm = maps.Clone(oldPlain), maps.Clone(oldPerm)
	candidates := make(map[string]struct{}, len(changed)*2)
	for p := range changed {
		for {
			if _, seen := candidates[p]; seen {
				break // 祖先链从这里往上已经登记过
			}
			candidates[p] = struct{}{}
			if p == "." || p == "" {
				break
			}
			p = filepath.Dir(p)
		}
	}

	err = DB.View(func(tx *bolt.Tx) error {
		var dirs []string
		for p := range candidates {
			n, err := nodeByPathTx(tx, p)
			if errors.Is(err, errNodeNotFound) || (err == nil && !n.IsDir) {
				// 不在树里了，或者已从目录变成文件：它和子孙目录的 rollup 一并作废
				if _, cached := plain[p]; cached {
					dropSubtree(plain, perm, p)
				}
				continue
			}
			if err != nil {
				return err
			}
			dirs = append(dirs, p)
		}
		// 深的先算：父目录要用到子目录刚算好的值
		sort.Slice(dirs, func(i, j int) bool { return depthOf(dirs[i]) > depthOf(dirs[j]) })
		var kids []Node
		for _, d := range dirs {
			var err error
			if kids, err = dirContentsTx(tx, d, kids[:0]); err != nil {
				return err
			}
			ptrs := make([]*Node, len(kids))
			for i := range kids {
				if kids[i].IsDir {
					if _, have := plain[kids[i].Path]; !have {
						return nil // ok 仍为 false
					}
				}
				ptrs[i] = &kids[i]
			}
			plain[d] = rollupOf(ptrs, func(p string) string { return plain[p] }, false)
			perm[d] = rollupOf(ptrs, func(p string) string { return perm[p] }, true)
		}
		ok = true
		return nil
	})
	return plain, perm, ok, err
}

// dropSubtree 从两份 rollup 里删掉 dir 及其全部子孙目录
func dropSubtree(plain, perm map[string]string, dir string) {
	prefix := dir + string(filepath.Separator)
	for p := range plain {
		if p == dir || strings.HasPrefix(p, prefix) {
			delete(plain, p)
			delete(perm, p)
		}
	}
}

func depthOf(p string) int {
	if p == "." || p == "" {
		return 0
	}
	return strings.Count(p, string(filepath.Separator)) + 1
}

// computeDirHashes 不含权限的 rollup（线格式 Node.Hash 的口径）
func computeDirHashes(nodes map[string]*Node) map[string]string {
	return computeRollups(nodes, false)
}

// computeRollups 从 path→Node 全量映射一趟算出所有目录的 rollup 哈希。
// 自底向上递归 + 记忆化：每个目录只算一次，总复杂度 O(N)。withPerm 为假时
// 与 v2.5.0 逐字节一致
func computeRollups(nodes map[string]*Node, withPerm bool) map[string]string {
	// 按父目录分组子节点；同时登记所有目录（空目录也要有 rollup）
	childrenByParent := make(map[string][]*Node, len(nodes))
	dirs := make([]string, 0, len(nodes))
	for p, n := range nodes {
		if n.IsDir {
			if _, ok := childrenByParent[p]; !ok {
				childrenByParent[p] = nil // 确保空目录键存在
			}
			dirs = append(dirs, p)
		}
		if p == "." || p == "" {
			continue // 根没有父，不入任何 children 列表
		}
		parent := filepath.Dir(p)
		childrenByParent[parent] = append(childrenByParent[parent], n)
	}

	result := make(map[string]string, len(dirs))
	var rollup func(dir string) string
	rollup = func(dir string) string {
		if h, ok := result[dir]; ok {
			return h
		}
		sum := rollupOf(childrenByParent[dir], rollup, withPerm)
		result[dir] = sum
		return sum
	}
	for _, d := range dirs {
		rollup(d)
	}
	return result
}

// rollupOf 由一个目录的直接子项算出它的 rollup；子目录的 rollup 经 childRollup 取得。
// 会就地按 Name 排序 kids
func rollupOf(kids []*Node, childRollup func(path string) string, withPerm bool) string {
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
	h := blake3.New()
	for _, k := range kids {
		// 每条：Name \0 类型 \0 (目录:子rollup | 文件:Size \0 Hash) \0
		_, _ = h.Write([]byte(k.Name))
		_, _ = h.Write([]byte{0})
		if k.IsDir {
			_, _ = h.Write([]byte{'d', 0})
			_, _ = h.Write([]byte(childRollup(k.Path)))
		} else {
			_, _ = h.Write([]byte{'f', 0})
			_, _ = h.Write([]byte(strconv.FormatUint(k.Size, 10)))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(k.Hash))
		}
		if m := EffectiveMode(k.IsDir, k.Mode); withPerm && m != 0 {
			_, _ = h.Write([]byte{0, 'm'})
			_, _ = h.Write([]byte(strconv.FormatUint(uint64(m), 8)))
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

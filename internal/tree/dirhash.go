package tree

import (
	"encoding/hex"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/zeebo/blake3"
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

// treeGen 单调递增的"树代际"计数：nodes 桶每次成功增删（AddNodes / DeleteNodes）都自增。
// DirHashes 以它作记忆化键——树没变就复用上次算好的哈希图，避免每次目录树请求都 O(N) 重算；
// 树一变缓存即失效，绝不会把陈旧 rollup 当新的返回（否则会漏剪或误剪）。
var treeGen atomic.Uint64

// bumpTreeGen 在一次成功的树变更后调用，使 DirHashes 的记忆化缓存失效
func bumpTreeGen() { treeGen.Add(1) }

var (
	dirHashMu        sync.Mutex
	dirHashCache     map[string]string // path → 该目录子树的 rollup 哈希（十六进制，不含权限）
	permDirHashCache map[string]string // 同上，含权限
	dirHashGen       uint64            // 算出缓存时的 treeGen
	dirHashValid     bool              // 是否已算过一次（与"gen 恰好为 0"区分）
)

// DirHashes 返回每个目录路径到其子树 rollup 哈希（不含权限）的映射（键为本地分隔符路径，
// 根为 "."）。以 treeGen 记忆化：树自上次计算以来没变则直接返回缓存，变了则整树重算一次。
// 并发安全（多客户端 goroutine 可同时调用）。返回的 map 只读、不可修改。
func DirHashes() (map[string]string, error) {
	plain, _, err := dirRollups()
	return plain, err
}

// PermDirHashes 同 DirHashes，但 rollup 计入权限位
func PermDirHashes() (map[string]string, error) {
	_, perm, err := dirRollups()
	return perm, err
}

func dirRollups() (plain, perm map[string]string, err error) {
	dirHashMu.Lock()
	defer dirHashMu.Unlock()

	gen := treeGen.Load()
	if dirHashValid && dirHashGen == gen && dirHashCache != nil {
		return dirHashCache, permDirHashCache, nil
	}

	nodes, err := LoadAllNodesByPath()
	if err != nil {
		return nil, nil, err
	}
	dirHashCache = computeRollups(nodes, false)
	permDirHashCache = computeRollups(nodes, true)
	dirHashGen = gen
	dirHashValid = true
	return dirHashCache, permDirHashCache, nil
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
		kids := childrenByParent[dir]
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		h := blake3.New()
		for _, k := range kids {
			// 每条：Name \0 类型 \0 (目录:子rollup | 文件:Size \0 Hash) \0
			_, _ = h.Write([]byte(k.Name))
			_, _ = h.Write([]byte{0})
			if k.IsDir {
				_, _ = h.Write([]byte{'d', 0})
				_, _ = h.Write([]byte(rollup(k.Path)))
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
		sum := hex.EncodeToString(h.Sum(nil))
		result[dir] = sum
		return sum
	}
	for _, d := range dirs {
		rollup(d)
	}
	return result
}

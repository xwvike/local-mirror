package tree

import (
	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

// Batch 接收端处理一个目录期间的树写入，按序攒起来在一个事务里提交。
// 逐文件提交时每个文件都要付一次数据库落盘（macOS 上是 F_FULLFSYNC，约 9 ms），
// 10 万个小文件的首次同步因此要多花二十多分钟。
//
// 作用域限于单个目录：处理该目录时读到的只有目录自身与其已有内容，都已在处理上一层时
// 提交，所以排队期间的写入不需要对读取可见。
//
// 写盘顺序：提交前先让本批文件数据落盘（SyncFileData + syncQueuedData），库里记录的
// 文件内容因此一定已在盘上。崩溃时丢的只是未提交的一批：这些文件磁盘上有、库里没有，
// 重启时 BuildFileTree 校准重算其哈希入库，与上游不一致的在下一轮 diff 中重新下载。
type Batch struct {
	ops     []batchOp
	data    bool
	changed []string
}

type batchOp struct {
	add *Node
	del string
}

// batchMaxOps 单批上限：大目录分几次提交，限制内存占用与崩溃时需要补回的量
const batchMaxOps = 256

// flushData 提交前让本批文件数据落盘（按平台实现，测试可替换以检验先后顺序）
var flushData = syncQueuedData

// Add 排队写入一个节点。hasData 表示对应文件刚写入了新内容，提交前需落盘
func (b *Batch) Add(node *Node, hasData bool) {
	b.ops = append(b.ops, batchOp{add: node})
	b.data = b.data || hasData
	b.commitIfFull()
}

// Delete 排队删除一个节点及其子树
func (b *Batch) Delete(path string) {
	b.ops = append(b.ops, batchOp{del: path})
	b.commitIfFull()
}

// ChangedDir 登记一个变更目录，提交后再写入变更日志：中继下游被唤醒时读到的必须是已提交的树
func (b *Batch) ChangedDir(dir string) {
	b.changed = append(b.changed, dir)
}

func (b *Batch) commitIfFull() {
	if len(b.ops) >= batchMaxOps {
		b.Commit()
	}
}

// Commit 提交排队的写入。整批事务失败时逐条重试，只丢弃自身出错的写入：一条坏记录
// （如损坏的 children 数据）不能连累同批其余写入。被丢弃的节点等同于未入库，
// 由下一轮 diff 或启动校准补回
func (b *Batch) Commit() {
	if len(b.ops) > 0 {
		if b.data {
			if err := flushData(); err != nil {
				log.Warnf("flushing downloaded file data to disk failed: %v", err)
			}
		}
		if err := update(func(tx *bolt.Tx) error {
			for _, op := range b.ops {
				if err := op.apply(tx); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			var failed int
			var first error
			for _, op := range b.ops {
				if err := update(op.apply); err != nil {
					failed++
					if first == nil {
						first = err
					}
				}
			}
			if failed > 0 {
				log.Errorf("%d of %d tree updates could not be recorded and will be redone by a later scan: %v",
					failed, len(b.ops), first)
			}
		}
	}
	for _, d := range b.changed {
		AddRecentChangedDir(d)
	}
	b.ops, b.data, b.changed = nil, false, nil
}

func (op batchOp) apply(tx *bolt.Tx) error {
	if op.add != nil {
		return addNodesTx(tx, []*Node{op.add})
	}
	return deleteNodesTx(tx, []string{op.del})
}

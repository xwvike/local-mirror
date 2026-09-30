package tree

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// InitialSync 接收端首次同步的进度记录，存于 meta 桶。
// pending：尚未完成首次同步；running：首次全量推送进行中（Cursor 为最后一个已提交的条目）；
// finalizing：推送已结束，待一次全量比对收尾；done：首次同步已完成
type InitialSync struct {
	State       string `json:"state"`
	Cursor      string `json:"cursor,omitempty"`
	Files       uint64 `json:"files,omitempty"`
	Bytes       uint64 `json:"bytes,omitempty"`
	TotalFiles  uint64 `json:"total_files,omitempty"`
	TotalBytes  uint64 `json:"total_bytes,omitempty"`
	UpdatedUnix int64  `json:"updated_unix,omitempty"`
}

const (
	InitialSyncPending    = "pending"
	InitialSyncRunning    = "running"
	InitialSyncFinalizing = "finalizing"
	InitialSyncDone       = "done"
)

const initialSyncKey = "initial_sync"

func (s InitialSync) encode() []byte {
	s.UpdatedUnix = time.Now().Unix()
	b, _ := json.Marshal(s)
	return b
}

// LoadInitialSync 读取首次同步记录
func LoadInitialSync() (InitialSync, error) {
	var s InitialSync
	err := DB.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("meta")).Get([]byte(initialSyncKey))
		if data == nil {
			return fmt.Errorf("initial sync record missing")
		}
		return json.Unmarshal(data, &s)
	})
	return s, err
}

// SaveInitialSync 立即写入首次同步记录
func SaveInitialSync(s InitialSync) error {
	return DB.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte(initialSyncKey), s.encode())
	})
}

// SetInitialSync 登记本批提交时一并写入的首次同步记录（以最后一次登记为准）。
// 与节点写在同一个事务里，续推游标因此永远与已入库的内容一致
func (b *Batch) SetInitialSync(s InitialSync) {
	b.meta = s.encode()
}

// initInitialSync 在 InitDB 的事务里确定记录初值：新建的库尚未同步过（pending）；
// 旧版本留下的库没有这条记录，其数据已由逐文件同步建立，视为已完成（done），
// 升级后不会触发整树重推
func initInitialSync(meta *bolt.Bucket, fresh bool) error {
	if meta.Get([]byte(initialSyncKey)) != nil {
		return nil
	}
	state := InitialSyncDone
	if fresh {
		state = InitialSyncPending
	}
	return meta.Put([]byte(initialSyncKey), InitialSync{State: state}.encode())
}

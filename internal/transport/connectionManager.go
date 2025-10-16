package transport

import (
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	Waiting    uint8 = 0x00 // 等待
	Online     uint8 = 0x01 // 在线
	Offline    uint8 = 0x02 // 离线
	Deprecated uint8 = 0x03 // 废弃
)

type ConnectionManager struct {
	conn        net.Conn
	mutex       sync.RWMutex
	connectAddr string
	maxRetries  int
	retryDelay  time.Duration
}

func NewConnectionManager(addr string) (*ConnectionManager, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	return &ConnectionManager{
		connectAddr: addr,
		maxRetries:  3,
		retryDelay:  3 * time.Second,
		conn:        conn,
	}, nil
}

func (cm *ConnectionManager) GetConnection() (net.Conn, error) {
	cm.mutex.RLock()
	if cm.conn != nil {
		if cm.isConnValid() {
			defer cm.mutex.RUnlock()
			return cm.conn, nil
		}
	}
	cm.mutex.RUnlock()
	return nil, fmt.Errorf("connection is invalid")
}

// todo: 需要添加使用心跳检测连接是否有效
func (cm *ConnectionManager) isConnValid() bool {
	return true
}

func (cm *ConnectionManager) Reconnect() error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}

	var err error
	for i := 0; i < cm.maxRetries; i++ {
		log.Infof("Attempting to reconnect (attempt %d/%d)", i+1, cm.maxRetries)

		cm.conn, err = net.Dial("tcp", cm.connectAddr)
		if err == nil {
			log.Info("Reconnection successful")
			return nil
		}

		log.Errorf("Reconnection attempt %d failed: %v", i+1, err)
		if i < cm.maxRetries-1 {
			time.Sleep(cm.retryDelay)
		}
	}

	return err
}

func (cm *ConnectionManager) Close() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}
}

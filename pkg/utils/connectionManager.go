package utils

import (
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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

// todo: 需要改成使用心跳检测连接是否有效
func (cm *ConnectionManager) isConnValid() bool {
	if cm.conn == nil {
		return false
	}
	cm.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	defer cm.conn.SetReadDeadline(time.Time{})

	one := make([]byte, 1)
	_, err := cm.conn.Read(one)

	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	return err == nil
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

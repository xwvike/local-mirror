package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/network"
	"time"

	log "github.com/sirupsen/logrus"
)

func InitConn() (*network.FileClient, error) {
	maxRetries := 3
	fileClient := network.NewFileClient(*config.RealityIP+":52345", "default")
	for i := 0; i < maxRetries; i++ {
		log.Warnf("Attempting to connect to server at %s, attempt %d/%d", fileClient.RealityAddr, i+1, maxRetries)
		err := fileClient.Handshake()
		if err != nil {
			log.Warnf("Handshake failed, attempt %d/%d: %v", i+1, maxRetries, err)
			if i == maxRetries-1 {
				fileClient.State = network.Offline
				return fileClient, err
			}
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		break
	}
	if fileClient.State != network.Online {
		return nil, fmt.Errorf("failed to establish connection with the server at %s", fileClient.RealityAddr)
	}
	return fileClient, nil
}

package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/network"
	"local-mirror/internal/transport"
	"time"

	log "github.com/sirupsen/logrus"
)

func InitConn() (*network.FileClient, error) {
	maxRetries := 3
	fileClient, err := network.NewFileClient(*config.RealityIP+":52345", "default")
	if err != nil {
		return fileClient, fmt.Errorf("failed to create file client: %w", err)
	}
	for i := range make([]struct{}, maxRetries) {
		log.Warnf("Attempting to connect to server at %s, attempt %d/%d", fileClient.RealityAddr, i+1, maxRetries)
		log.Debugf("Connecting to server at %s", fileClient.RealityAddr)
		err := fileClient.Handshake()
		if err != nil {
			log.Warnf("Handshake failed, attempt %d/%d: %v", i+1, maxRetries, err)
			if i == maxRetries-1 {
				fileClient.State = transport.Deprecated
				return fileClient, err
			}
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		break
	}
	if fileClient.State != transport.Online {
		return fileClient, fmt.Errorf("failed to establish connection with the server at %s", fileClient.RealityAddr)
	}
	return fileClient, nil
}

package app

import (
	"local-mirror/internal/network"
	"local-mirror/internal/tree"
	"time"

	"fmt"
	log "github.com/sirupsen/logrus"
)

func Reality() {
	log.Debug("step 3 >> start file server")
	fileServer := network.NewFileServer("0.0.0.0:52345")
	if err := fileServer.Start(); err != nil {
		log.Fatal("Error starting file server:", err)
	}
	go func() {
		tictik := time.NewTicker(10 * time.Second)
		defer tictik.Stop()
		for range tictik.C {
			fmt.Println("change", tree.RecentChangedDirs)
		}
	}()
}

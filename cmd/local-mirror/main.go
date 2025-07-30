package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal"
	"local-mirror/internal/logger"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

func init() {
	config.InstanceID = utils.GenerateRandomNum()
	config.StartTime = time.Now().Unix()
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("获取当前执行文件路径失败: %v", err)
		os.Exit(1)
	}
	fmt.Print(wd)
	config.StartPath = wd
}
func main() {
	defer tree.DB.Close()
	flag.Parse()
	logger.InitLogger()
	log.Infof("实例ID: %x", config.InstanceID)
	log.Infof("协议版本: %x", config.Version)
	log.Infof("运行模式: %s", *config.Mode)
	log.Infof("日志级别: %s", *config.LogLevel)
	log.Infof("启动时间: %d", config.StartTime)
	log.Infof("当前工作目录: %s", config.StartPath)
	tree.InitDB()
	app.App()
}

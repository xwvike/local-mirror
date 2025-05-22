package main

import (
	"flag"
	"local-mirror/config"
	"local-mirror/internal/app"
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
	config.StartPath = wd
}
func main() {
	flag.Parse()
	app.InitLogger()
	log.Debugf("实例ID: %x", config.InstanceID)
	log.Debugf("协议版本: %x", config.Version)
	log.Debugf("运行模式: %s", *config.Mode)
	log.Debugf("日志级别: %s", *config.LogLevel)
	log.Debugf("启动时间: %d", config.StartTime)
	log.Debugf("当前工作目录: %s", config.StartPath)
	app.App()
}

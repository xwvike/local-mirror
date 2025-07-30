package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	app "local-mirror/internal"
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

	// 处理帮助和版本信息
	if *config.Help {
		flag.Usage()
		os.Exit(0)
	}

	if *config.Version {
		fmt.Printf("Local Mirror version 1.0.0\n")
		fmt.Printf("Protocol version: 0x%04X\n", config.ProtocolVersion)
		fmt.Printf("Build date: %s\n", "2025-07-30")
		fmt.Printf("Go version: %s\n", "go1.21+")
		fmt.Printf("\nCopyright (c) 2025 Local Mirror Team\n")
		fmt.Printf("Licensed under MIT License\n")
		os.Exit(0)
	}

	logger.InitLogger()
	log.Infof("实例ID: %x", config.InstanceID)
	log.Infof("协议版本: %x", config.ProtocolVersion)
	log.Infof("运行模式: %s", *config.Mode)
	log.Infof("日志级别: %s", *config.LogLevel)
	log.Infof("启动时间: %d", config.StartTime)
	log.Infof("当前工作目录: %s", config.StartPath)
	tree.InitDB()
	app.App()
}

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// 远程服务器配置
	remoteHost    = "192.168.100.10"                     // 替换为你的 Debian 主机地址
	remoteUser    = "xwvike"                             // 替换为你的 Debian 用户名（确保不是root）
	remotePort    = "22"                                 // SSH 端口
	remoteBinPath = "/tmp/local-mirror"                  // 远程二进制文件路径
	sshKeyPath    = "/Users/xiazhike/.ssh/debian_xwvike" // SSH私钥路径，根据实际情况修改

	// 本地配置
	localBuildPath = "./dist/local-mirror" // 本地构建路径
)

func main() {
	log.Println("开始 TCP 连接测试...")

	// 1. 构建二进制文件
	if err := buildBinary(); err != nil {
		log.Fatalf("构建失败: %v", err)
	}
	log.Println("二进制文件构建成功")

	// 2. 运行本地测试客户端
	if err := runLocalClient(); err != nil {
		log.Fatalf("本地客户端测试失败: %v", err)
	}
	log.Println("本地客户端测试完成")

	// 3. 传输二进制文件到远程服务器
	if err := transferBinary(); err != nil {
		log.Fatalf("传输失败: %v", err)
	}
	log.Println("二进制文件传输成功")

	// 4. 在远程服务器上启动服务
	if err := startRemoteServer(); err != nil {
		log.Fatalf("启动远程服务失败: %v", err)
	}
	log.Println("远程服务已启动")

	// 等待服务器启动
	log.Println("等待服务器就绪...")
	time.Sleep(3 * time.Second)

	// 5. 停止远程服务并清理
	if err := stopAndCleanup(); err != nil {
		log.Fatalf("清理失败: %v", err)
	}
	log.Println("远程服务已停止，并完成清理")

	log.Println("TCP 连接测试完成!")
}

func buildBinary() error {
	cmd := exec.Command("go", "build", "-o", localBuildPath, "./cmd/main/main.go")
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH=amd64",
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func transferBinary() error {
	// 使用 sftp 传输二进制文件，添加密钥参数
	cmd := exec.Command("scp", "-P", remotePort, "-i", sshKeyPath, localBuildPath, fmt.Sprintf("%s@%s:%s", remoteUser, remoteHost, remoteBinPath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func startRemoteServer() error {
	logFile, err := os.Create("client.log")
	if err != nil {
		return fmt.Errorf("无法创建日志文件: %v", err)
	}
	// 使用 SSH 远程启动服务器，添加密钥参数
	sshCmd := fmt.Sprintf("chmod +x %s && nohup %s -mode=mirror -logLevel=debug > /dev/null 2>&1 &", remoteBinPath, remoteBinPath)
	fmt.Println("sshCmd:", sshCmd)
	cmd := exec.Command("ssh", "-p", remotePort, "-i", sshKeyPath, fmt.Sprintf("%s@%s", remoteUser, remoteHost), sshCmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd.Start()
}

func runLocalClient() error {
	// 运行本地客户端测试
	logFile, err := os.Create("server.log")
	if err != nil {
		return fmt.Errorf("无法创建日志文件: %v", err)
	}
	cmd := exec.Command("go", "run", "./cmd/main/main.go", "-mode=reality", "-logLevel=debug")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd.Start()
}

func stopAndCleanup() error {
	// 停止远程服务并删除二进制文件，添加密钥参数
	sshCmd := fmt.Sprintf("pkill -f '%s'", filepath.Base(remoteBinPath))
	fmt.Println("sshCmd:", sshCmd)
	cmd := exec.Command("ssh", "-p", remotePort, "-i", sshKeyPath, fmt.Sprintf("%s@%s", remoteUser, remoteHost), sshCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

package main

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SystemLimits 包含系统的各种限制
type SystemLimits struct {
	MaxOpenFiles    uint64 // 文件描述符限制
	MaxInotifyWatch uint64 // inotify watchers 限制 (Linux/macOS)
}

// getSystemLimits 获取系统限制
func getSystemLimits() *SystemLimits {
	limits := &SystemLimits{
		MaxOpenFiles:    getMaxOpenFiles(),
		MaxInotifyWatch: getMaxInotifyWatchers(),
	}
	return limits
}

// getMaxOpenFiles 获取文件描述符限制
func getMaxOpenFiles() uint64 {
	if runtime.GOOS == "windows" {
		return 65535 // Windows has a high limit by default
	}

	var rLimit unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		// 降级处理，返回一个安全的默认值
		switch runtime.GOOS {
		case "darwin":
			return 10240 // macOS default
		case "linux":
			return 4096 // Linux conservative default
		default:
			return 1024 // Very conservative default
		}
	}
	return rLimit.Cur
}

// getMaxInotifyWatchers 获取 inotify watchers 限制
func getMaxInotifyWatchers() uint64 {
	switch runtime.GOOS {
	case "windows":
		// Windows 使用不同的文件监听机制，没有 inotify 限制
		return 65535
	case "linux":
		return getLinuxInotifyLimit()
	case "darwin":
		return getMacOSKqueueLimit()
	default:
		return 1024 // 保守默认值
	}
}

// getLinuxInotifyLimit 获取 Linux inotify 限制
func getLinuxInotifyLimit() uint64 {
	// 尝试读取系统限制 - 按优先级排序
	limitSources := []struct {
		path        string
		description string
	}{
		{"/proc/sys/fs/inotify/max_user_watches", "用户可创建的 watch 数量"},
		{"/proc/sys/fs/inotify/max_user_instances", "用户可创建的 inotify 实例数"},
	}

	for _, source := range limitSources {
		if limit := readIntFromFile(source.path); limit > 0 {
			// max_user_watches 是我们真正关心的限制
			if strings.Contains(source.path, "max_user_watches") {
				return limit
			}
			// max_user_instances 需要考虑每个实例可能有多个 watch
			// 保守估计每个实例平均 100 个 watch
			if strings.Contains(source.path, "max_user_instances") {
				return limit * 100
			}
		}
	}

	// 尝试通过 ulimit 获取 (某些系统)
	if limit := getLinuxUlimitInotify(); limit > 0 {
		return limit
	}

	// 如果无法读取，返回 Linux 默认值
	// 大多数现代 Linux 发行版默认是 8192
	return 8192
}

// getLinuxUlimitInotify 尝试通过 ulimit 或其他方式获取限制
func getLinuxUlimitInotify() uint64 {
	// 某些 Linux 发行版可能通过其他方式设置限制
	// 尝试读取 systemd 或其他配置

	possiblePaths := []string{
		"/etc/security/limits.conf",
		"/etc/systemd/user.conf",
		"/etc/systemd/system.conf",
	}

	for _, path := range possiblePaths {
		if limit := parseConfigFile(path, "inotify"); limit > 0 {
			return limit
		}
	}

	return 0
}

// parseConfigFile 解析配置文件中的限制设置
func parseConfigFile(path, keyword string) uint64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}

		// 查找包含关键词的行
		if strings.Contains(strings.ToLower(line), keyword) {
			// 简单的数字提取
			fields := strings.Fields(line)
			for _, field := range fields {
				if value, err := strconv.ParseUint(field, 10, 64); err == nil && value > 0 {
					return value
				}
			}
		}
	}

	return 0
}

// getMacOSKqueueLimit 获取 macOS kqueue 限制
func getMacOSKqueueLimit() uint64 {
	// macOS 使用 kqueue，限制通常与文件描述符相关
	// 但实际限制可能更复杂，这里尝试获取 kern.maxfiles

	// 方法1: 尝试通过 syscall 直接调用 sysctl
	if limit := getMacOSSysctlBySyscall("kern.maxfilesperproc"); limit > 0 {
		// kqueue 监听器通常占用文件描述符，但不是1:1关系
		// 保守估计使用 80% 的文件描述符限制
		return uint64(float64(limit) * 0.8)
	}

	// 方法2: 尝试通过命令行 sysctl 获取
	if limit := getMacOSSysctlByCommand("kern.maxfilesperproc"); limit > 0 {
		return uint64(float64(limit) * 0.8)
	}

	// 方法3: 获取 kern.maxfiles (系统全局限制)
	if limit := getMacOSSysctlBySyscall("kern.maxfiles"); limit > 0 {
		// 系统全局限制，保守使用 10%
		return uint64(float64(limit) * 0.1)
	}

	// 方法4: 降级到文件描述符限制的一半
	fileLimit := getMaxOpenFiles()
	if fileLimit > 0 {
		return fileLimit / 2
	}

	// 最后的默认值
	return 4096
}

// getMacOSSysctlBySyscall 通过系统调用获取 macOS sysctl 参数
func getMacOSSysctlBySyscall(name string) uint64 {
	// 将参数名转换为 MIB (Management Information Base)
	mib, err := sysctlnametomib(name)
	if err != nil {
		return 0
	}

	// 获取值的大小
	var valueSize uintptr
	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		0, // 不获取值，只获取大小
		uintptr(unsafe.Pointer(&valueSize)),
		0,
		0,
	)
	if errno != 0 {
		return 0
	}

	// 分配缓冲区并获取实际值
	value := make([]byte, valueSize)
	_, _, errno = syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		uintptr(unsafe.Pointer(&value[0])),
		uintptr(unsafe.Pointer(&valueSize)),
		0,
		0,
	)
	if errno != 0 {
		return 0
	}

	// 根据数据类型解析结果
	// kern.maxfiles* 通常是 int 或 long
	if valueSize == 4 {
		return uint64(*(*uint32)(unsafe.Pointer(&value[0])))
	} else if valueSize == 8 {
		return *(*uint64)(unsafe.Pointer(&value[0]))
	}

	return 0
}

// sysctlnametomib 将 sysctl 名称转换为 MIB
func sysctlnametomib(name string) ([]int32, error) {
	// 首先获取 MIB 的长度
	var mibLen uintptr = 24 // CTL_MAXNAME 的最大值
	mib := make([]int32, mibLen)

	nameBytes, err := syscall.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_SYSCTLNAMETOMIB,
		uintptr(unsafe.Pointer(nameBytes)),
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(unsafe.Pointer(&mibLen)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return nil, errno
	}

	return mib[:mibLen], nil
}

// getMacOSSysctlByCommand 通过命令行调用 sysctl (备用方法)
func getMacOSSysctlByCommand(name string) uint64 {
	cmd := exec.Command("sysctl", "-n", name)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0
	}

	return value
}

// readIntFromFile 从文件读取整数值
func readIntFromFile(path string) uint64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if value, err := strconv.ParseUint(text, 10, 64); err == nil {
			return value
		}
	}
	return 0
}

// 删除了简化的 cgo 注释部分，现在提供实际的实现

// getOptimalWatcherLimit 获取建议的监听器数量限制
func getOptimalWatcherLimit() uint64 {
	limits := getSystemLimits()

	// 使用更保守的限制，为其他程序留出空间
	watcherLimit := limits.MaxInotifyWatch
	if watcherLimit > 1000 {
		// 保留 20% 的空间给系统和其他程序
		watcherLimit = uint64(float64(watcherLimit) * 0.8)
	}

	// 确保不超过文件描述符限制的一半
	fileLimit := limits.MaxOpenFiles / 2
	if watcherLimit > fileLimit {
		watcherLimit = fileLimit
	}

	// 设置最小值
	if watcherLimit < 100 {
		watcherLimit = 100
	}

	return watcherLimit
}

// 使用示例和测试函数
func main() {
	limits := getSystemLimits()
	optimalLimit := getOptimalWatcherLimit()

	println("System Information:")
	println("OS:", runtime.GOOS)
	println("Max Open Files:", limits.MaxOpenFiles)
	println("Max Inotify Watchers:", limits.MaxInotifyWatch)
	println("Recommended Watcher Limit:", optimalLimit)

	// 详细信息 (调试用)
	if runtime.GOOS == "darwin" {
		println("\nmacOS specific limits:")
		if limit := getMacOSSysctlBySyscall("kern.maxfilesperproc"); limit > 0 {
			println("kern.maxfilesperproc (syscall):", limit)
		}
		if limit := getMacOSSysctlByCommand("kern.maxfilesperproc"); limit > 0 {
			println("kern.maxfilesperproc (command):", limit)
		}
		if limit := getMacOSSysctlBySyscall("kern.maxfiles"); limit > 0 {
			println("kern.maxfiles (syscall):", limit)
		}
	} else if runtime.GOOS == "linux" {
		println("\nLinux specific limits:")
		if limit := readIntFromFile("/proc/sys/fs/inotify/max_user_watches"); limit > 0 {
			println("max_user_watches:", limit)
		}
		if limit := readIntFromFile("/proc/sys/fs/inotify/max_user_instances"); limit > 0 {
			println("max_user_instances:", limit)
		}
	}
}

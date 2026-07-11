package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"local-mirror/config"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 监督进程模式：--config 指定 YAML 后，父进程为每个任务 re-exec 自身
// 为子进程（一任务一进程，就是久经验证的单实例模型），负责日志汇聚、
// 退避重启与信号转发。任务间零共享，bbolt 按目录锁天然隔离。
const (
	superviseBaseDelay = 5 * time.Second  // 首次重启退避
	superviseMaxDelay  = 60 * time.Second // 退避上限（与 Mirror 重连策略一致）
	healthyRunReset    = 60 * time.Second // 运行超过此时长视为健康，退避归零
	shutdownGrace      = 5 * time.Second  // SIGTERM 后等待子进程退出的宽限期
)

// childRef 供信号转发读取当前存活的子进程句柄
type childRef struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

func (r *childRef) set(c *exec.Cmd) { r.mu.Lock(); r.cmd = c; r.mu.Unlock() }

func (r *childRef) signal(sig os.Signal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(sig)
	}
}

// runSupervisor 阻塞运行直到收到关停信号（exit 0）或全部任务永久失败（exit 1）
func runSupervisor(cfg *config.MultiConfig) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: 无法确定自身可执行文件路径: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Local Mirror %s · 多任务监督模式（%d 个任务）\n", version, len(cfg.Tasks))
	for _, t := range cfg.Tasks {
		fmt.Printf("  [%s] %s  %s\n", t.Name, t.Mode, t.Path)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refs := make([]*childRef, len(cfg.Tasks))
	var wg sync.WaitGroup
	for i := range cfg.Tasks {
		refs[i] = &childRef{}
		wg.Add(1)
		go func(t config.TaskConfig, ref *childRef) {
			defer wg.Done()
			superviseTask(ctx, exe, t, ref)
		}(cfg.Tasks[i], refs[i])
	}

	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		fmt.Fprintln(os.Stderr, "local-mirror: 收到关停信号，正在停止全部任务…")
		cancel()
		for _, r := range refs {
			r.signal(syscall.SIGTERM)
		}
		select {
		case <-allDone:
		case <-time.After(shutdownGrace):
			// 宽限期内未退出的强杀
			for _, r := range refs {
				r.signal(syscall.SIGKILL)
			}
			<-allDone
		}
		os.Exit(0)
	case <-allDone:
		// 所有管理 goroutine 自然退出 = 每个任务都永久失败
		fmt.Fprintln(os.Stderr, "local-mirror: 所有任务均已永久失败，退出")
		os.Exit(1)
	}
}

// superviseTask 管理单个任务的生命周期：启动、日志转发、按退出码
// 决定重启（exit 2 = 配置/用法错误，永久失败不重启）
func superviseTask(ctx context.Context, exe string, t config.TaskConfig, ref *childRef) {
	delay := superviseBaseDelay
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		code, err := runTaskOnce(ctx, exe, t, ref)
		if ctx.Err() != nil {
			return // 父进程关停引起的退出，不算失败
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 启动失败: %v\n", t.Name, err)
		}
		if code == 2 {
			fmt.Fprintf(os.Stderr, "[%s] 退出码 2（配置/用法错误），不再重启\n", t.Name)
			return
		}
		if time.Since(start) > healthyRunReset {
			delay = superviseBaseDelay
		}
		fmt.Fprintf(os.Stderr, "[%s] 退出码 %d，%s 后重启\n", t.Name, code, delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(time.Duration(float64(delay)*1.5), superviseMaxDelay)
	}
}

// runTaskOnce 启动一次子进程并阻塞到其退出，返回退出码。
// stdout/stderr 按行加 [name] 前缀转发到父进程对应流
func runTaskOnce(ctx context.Context, exe string, t config.TaskConfig, ref *childRef) (int, error) {
	cmd := exec.Command(exe, taskArgs(t)...)
	// 口令绝不进 argv（ps 可见）：复用现有的 LOCAL_MIRROR_SECRET 环境变量
	// 机制（config 包将其作为 -k 的默认值）。任务未配置 secret 时不追加，
	// 继承父进程环境（允许在 systemd unit 里统一设置全局口令）
	cmd.Env = os.Environ()
	if t.Secret != "" {
		cmd.Env = append(cmd.Env, "LOCAL_MIRROR_SECRET="+t.Secret)
	}
	// stdin 显式空设备：子进程确定性走非 TTY 路径（发现、无色输出）
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	ref.set(cmd)
	defer ref.set(nil)

	var pipeWg sync.WaitGroup
	pipeWg.Add(2)
	go func() { defer pipeWg.Done(); prefixLines(stdout, os.Stdout, t.Name) }()
	go func() { defer pipeWg.Done(); prefixLines(stderr, os.Stderr, t.Name) }()
	pipeWg.Wait() // 管道读尽（子进程退出）后才能 Wait
	err = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		// 被信号杀死等无退出码的情况
		return -1, nil
	}
	return code, nil
}

func prefixLines(r io.Reader, w io.Writer, name string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fmt.Fprintf(w, "[%s] %s\n", name, sc.Text())
	}
}

func countRealityTasks(cfg *config.MultiConfig) int {
	n := 0
	for _, t := range cfg.Tasks {
		if t.Mode == "reality" || t.Mode == "relay" {
			n++
		}
	}
	return n
}

// taskArgs 把任务配置映射为子进程 argv（与单实例旗子一一对应）
func taskArgs(t config.TaskConfig) []string {
	args := []string{"-m", t.Mode, "-p", t.Path, "-a", t.Name}
	if t.LogLevel != "" {
		args = append(args, "-l", t.LogLevel)
	}
	if t.RealityIP != "" {
		args = append(args, "-r", t.RealityIP)
	}
	if len(t.Ignore) > 0 {
		args = append(args, "-i", strings.Join(t.Ignore, ","))
	}
	if t.AllowDelete {
		args = append(args, "--allow-delete")
	}
	if t.AllowCritical {
		args = append(args, "--allow-critical")
	}
	if t.CoolDown > 0 {
		args = append(args, "-c", strconv.FormatInt(t.CoolDown, 10))
	}
	if t.FileBufferSize > 0 {
		args = append(args, "-f", strconv.FormatUint(t.FileBufferSize, 10))
	}
	return args
}

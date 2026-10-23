package utils

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/zeebo/blake3"
)

type OSInfo struct {
	hostname     string
	UserHomeDir  string
	OS           string
	Architecture string
	NumCPU       int
}

func BaseOSInfo() *OSInfo {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		userHomeDir = "unknown"
	}
	return &OSInfo{
		hostname:     hostname,
		UserHomeDir:  userHomeDir,
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		NumCPU:       runtime.NumCPU(),
	}
}

func GenerateRandomNum() uint32 {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	if err != nil {
		panic("failed to generate random instance ID: " + err.Error())
	}
	return binary.BigEndian.Uint32(b)
}

func CalcBlake3(path string) ([32]byte, error) {
	var result [32]byte
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()

	hash := GetHasher()
	defer PutHasher(hash)
	buf := readBufPool.Get().(*[]byte)
	defer readBufPool.Put(buf)
	// 包一层藏起 *os.File 的 WriteTo：否则 io.CopyBuffer 走 WriteTo，内部每次另分配 32 KB，传进去的 buf 用不上
	if _, err := io.CopyBuffer(hash, struct{ io.Reader }{f}, *buf); err != nil {
		return result, err
	}

	copy(result[:], hash.Sum(nil))
	return result, nil
}

// 建树与传输时每个文件都要一个 BLAKE3 hasher（约 8 KB）和读缓冲，十万个小文件就是几个 GB 的
// 分配，GC 能吃掉发送端两成 CPU。两者都放进池里复用
var (
	hasherPool  = sync.Pool{New: func() any { return blake3.New() }}
	readBufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
)

// GetHasher 从池里取一个空白的 BLAKE3 hasher，用完交给 PutHasher
func GetHasher() *blake3.Hasher { return hasherPool.Get().(*blake3.Hasher) }

// PutHasher 重置 h 并放回池里；之后不得再用 h
func PutHasher(h *blake3.Hasher) {
	h.Reset()
	hasherPool.Put(h)
}

// HashString 计算字符串的 blake3 摘要（取前 16 字节的十六进制），
// 用于把任意路径映射为长度固定、文件系统安全的名字
func HashString(s string) string {
	h := blake3.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:16])
}

func RandomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b), nil
}

// RelPath 把绝对路径转换为相对同步根目录的规范形式：
// 根目录为 "."，其余为不带 "./" 前缀的相对路径（如 "subdir/a.txt"）。
// 全项目的节点路径必须统一走这里，与 filepath.Dir/Join 的清洗结果保持一致，
// 否则 "./subdir" 与 "subdir" 会被当成两个不同的键。
func RelPath(root, fullPath string) string {
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return fullPath
	}
	return rel
}

// IsIgnored 判断路径是否命中忽略列表。模式分两种，按去掉尾部斜杠后
// 是否仍含 "/" 区分：
//
//   - 不含 "/"：按路径段逐一匹配（任意深度）。"target" 命中 target 本身，
//     也命中 a/b/target。按段比较避免 strings.Contains 的误伤（"bin" 命中
//     "cabinet"）。尾部斜杠只表示"这是目录"，不影响归类："target/" 等同
//     "target"
//   - 含 "/"：按路径匹配，从同步根锚定（前导 "/" 可省）。"src-tauri/target"
//     只命中根下那一棵子树，不会误伤任意深度的同名目录。"**" 匹配任意多段
//     （含零段），需要任意深度时写 "**/src-tauri/target"
//
// 两种模式的段内都用 filepath.Match 比较，支持 * ? [] 通配符（如 *.log）。
// 匹配按前缀生效：模式命中路径的前若干段即算命中，所以命中一个目录等于
// 命中它整棵子树。大小写敏感。模式合法性在加载时已校验
// （config.LoadIgnoreList → ValidateIgnorePattern），此处 Match 出错按不匹配处理
func IsIgnored(relPath string, patterns []string) bool {
	segs := splitPathSegments(relPath)
	for _, p := range patterns {
		// 快路径：不含 "/" 的单段名字模式（默认项与绝大多数 -i 都是这种）
		// 直接按段比较，不解析、不分配。IsIgnored 在树构建时每个文件都要
		// 走一遍，这条路径的开销要和引入路径模式之前保持一致
		if strings.IndexByte(p, '/') < 0 {
			for _, seg := range segs {
				if ok, _ := filepath.Match(p, seg); ok {
					return true
				}
			}
			continue
		}
		pat, err := parseIgnorePattern(p)
		if err != nil {
			continue // 加载时已校验，此处按不匹配处理
		}
		if !pat.anchored { // 形如 "target/"：去掉尾部斜杠后仍是名字模式
			for _, seg := range segs {
				if ok, _ := filepath.Match(pat.name, seg); ok {
					return true
				}
			}
			continue
		}
		if matchPathPrefix(pat.segs, segs) {
			return true
		}
	}
	return false
}

// splitPathSegments 把相对路径切成非空段（丢掉 "" 与 "."）。
// 绝大多数路径本就没有空段，这时直接复用 Split 的结果，不额外分配
func splitPathSegments(relPath string) []string {
	raw := strings.Split(filepath.ToSlash(relPath), "/")
	keep := 0
	for _, s := range raw {
		if s != "" && s != "." {
			keep++
		}
	}
	if keep == len(raw) {
		return raw
	}
	segs := make([]string, 0, keep)
	for _, s := range raw {
		if s == "" || s == "." {
			continue
		}
		segs = append(segs, s)
	}
	return segs
}

// ignorePattern 规范化后的忽略模式
type ignorePattern struct {
	segs     []string // anchored 时的路径段
	name     string   // 非 anchored 时的单段名字（已去掉尾部斜杠）
	anchored bool
}

// parseIgnorePattern 规范化一条模式：去掉尾部斜杠（只表示"这是目录"，
// 不参与归类），再按是否仍含 "/" 决定匹配方式
func parseIgnorePattern(p string) (ignorePattern, error) {
	trimmed := strings.TrimSuffix(p, "/")
	if !strings.Contains(trimmed, "/") {
		if trimmed == "" {
			return ignorePattern{}, fmt.Errorf("pattern %q has no name", p)
		}
		return ignorePattern{name: trimmed}, nil
	}
	segs := splitPathSegments(strings.TrimPrefix(trimmed, "/"))
	if len(segs) == 0 {
		return ignorePattern{}, fmt.Errorf("pattern %q has no path segments", p)
	}
	return ignorePattern{segs: segs, anchored: true}, nil
}

// matchPathPrefix 模式段序列是否匹配路径段序列的一个前缀。
// "**" 吞掉任意多段（含零段）
func matchPathPrefix(pat, segs []string) bool {
	if len(pat) == 0 {
		return false // 空模式（如 "/" 或 "//"）不匹配任何东西
	}
	if pat[0] == "**" {
		rest := pat[1:]
		if len(rest) == 0 {
			return true // 以 ** 结尾：其后任意深度全部命中
		}
		for i := 0; i <= len(segs); i++ {
			if matchPathPrefix(rest, segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false // 路径比模式短：命中的是更深的层级，此路径本身不算
	}
	if ok, _ := filepath.Match(pat[0], segs[0]); !ok {
		return false
	}
	return len(pat) == 1 || matchPathPrefix(pat[1:], segs[1:])
}

// ValidateIgnorePattern 校验一条忽略模式的合法性（加载时调用一次）。
// 路径模式逐段校验，避免把整条含 "/" 的模式丢给 filepath.Match——
// 后者会把 "/" 当普通字符，语法错误反而检不出来
func ValidateIgnorePattern(p string) error {
	pat, err := parseIgnorePattern(p)
	if err != nil {
		return fmt.Errorf("invalid ignore pattern: %w", err)
	}
	if !pat.anchored {
		if _, err := filepath.Match(pat.name, "x"); err != nil {
			return fmt.Errorf("invalid ignore pattern %q: %w", p, err)
		}
		return nil
	}
	for _, s := range pat.segs {
		if _, err := filepath.Match(s, "x"); err != nil {
			return fmt.Errorf("invalid ignore pattern %q: bad segment %q: %w", p, s, err)
		}
	}
	return nil
}

func UniqueStrings(input []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(input))
	for _, v := range input {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

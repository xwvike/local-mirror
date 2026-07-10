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

	hash := blake3.New()
	if _, err := io.Copy(hash, f); err != nil {
		return result, err
	}

	copy(result[:], hash.Sum(nil))
	return result, nil
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

// IsIgnored 判断路径是否命中忽略列表。
// 按路径段逐一匹配（任意深度），避免 strings.Contains 造成的误伤
// （如 "bin" 匹配到 "cabinet"）。每段用 filepath.Match 比较，支持
// * ? [] 通配符（如 *.log）；纯名字模式退化为精确匹配。大小写敏感。
// 模式合法性在加载时已校验（config.LoadIgnoreList），此处 Match
// 出错按不匹配处理
func IsIgnored(relPath string, patterns []string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(relPath), "/") {
		if seg == "" || seg == "." {
			continue
		}
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, seg); ok {
				return true
			}
		}
	}
	return false
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

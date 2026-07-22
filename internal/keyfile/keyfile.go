// Package keyfile 管理 <同步根>/.local-mirror/key 密钥文件（公网化支柱 C，
// 见 docs/PUBLIC_EXPOSURE.md §C）。监听端 --gen-key 生成强随机 key，
// 拨号端显式 -k 时对称落盘；解析优先级：显式 -k ＞ 密钥文件 ＞ 明文。
// 放工作目录而非 ~/.config：.local-mirror 是强制忽略项（key 绝不会被同步）、
// 不依赖 $HOME、每根一把 key = 每链独立身份。
package keyfile

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
)

// KeyBytes 生成密钥的随机字节数。base64 后约 44 字符，对齐 wg genkey；
// 256 bit keyspace 让在线/离线爆破全免谈
const KeyBytes = 32

// Path 返回同步根下的密钥文件路径
func Path(root string) string {
	return filepath.Join(root, ".local-mirror", "key")
}

// Load 读取密钥文件。文件不存在返回 ("", nil)；存在但为空视为损坏报错，
// 避免"空 key 静默跑明文"
func Load(root string) (string, error) {
	data, err := os.ReadFile(Path(root))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read key file %s: %w", Path(root), err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("key file %s is empty (corrupt? regenerate with --gen-key --force)", Path(root))
	}
	return key, nil
}

// Generate 生成强随机 key 并写入密钥文件（600）。
// 已存在时拒绝覆盖——断链保护：监听端重生成 = 所有持旧 key 的拨号端失联，
// 须 force 显式确认
func Generate(root string, force bool) (string, error) {
	path := Path(root)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("key file already exists: %s\n"+
				"regenerating disconnects every dialer holding the old key; pass --force to overwrite", path)
		}
	}
	buf := make([]byte, KeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to gather randomness: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(buf)
	if err := write(path, key); err != nil {
		return "", err
	}
	return key, nil
}

// Save 把 key 落盘（拨号端对称持有，下次启动可省 -k）。
// 内容一致时不重写；损坏的旧文件直接覆盖自愈。返回是否实际写入
func Save(root, key string) (bool, error) {
	if existing, err := Load(root); err == nil && existing == key {
		return false, nil
	}
	if err := write(Path(root), key); err != nil {
		return false, err
	}
	return true, nil
}

func write(path, key string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create state dir for key file: %w", err)
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write key file %s: %w", path, err)
	}
	// WriteFile 不改已有文件的权限，显式收紧到 600（即便目录本身更宽）
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("failed to tighten key file permissions: %w", err)
	}
	return nil
}

// Fingerprint 供横幅/日志展示的 key 指纹（8 位十六进制）。
// 稳态输出只显指纹、绝不显 key；域分离前缀与 Noise PSK 派生互不重合
func Fingerprint(secret string) string {
	sum := blake3.Sum256([]byte("local-mirror-key-fingerprint-v1:" + secret))
	return hex.EncodeToString(sum[:4])
}

package utils

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"github.com/zeebo/blake3"
	"os"
	"runtime"
)

type OSInfo struct {
	hostname     string
	UserHomeDir  string
	OS           string
	Architecture string
	NumCPU       int
}

func BaseOSInfo() OSInfo {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		userHomeDir = "unknown"
	}
	return OSInfo{
		hostname:     hostname,
		UserHomeDir:  userHomeDir,
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		NumCPU:       runtime.NumCPU(),
	}
}

func StructToJson(data interface{}) (string, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(jsonData), nil
}

func GetSize(path string) (int64, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fileInfo.Size(), nil
}
func GetModTime(path string) (int64, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fileInfo.ModTime().Unix(), nil
}
func GetMode(path string) (os.FileMode, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fileInfo.Mode(), nil
}
func IsDir(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fileInfo.IsDir(), nil
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

// func ensureFileCanBeCreated(localPath string) (*os.File, error) {
//     dir := filepath.Dir(localPath)
//     if err := os.MkdirAll(dir, 0755); err != nil {
//         return nil, err
//     }
//     return os.Create(localPath)
// }

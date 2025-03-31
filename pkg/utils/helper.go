package utils

import (
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

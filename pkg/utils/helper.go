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

package utils

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"runtime"

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

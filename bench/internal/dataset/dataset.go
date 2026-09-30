// Package dataset 定义基准测试的固定数据集。文件布局、大小与内容全部由固定种子
// 决定，任何机器上生成的字节都相同，manifest 记录每个文件的 sha256 供校验。
package dataset

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version 数据集定义变更时递增，已生成的数据随之失效重建
const Version = 1

// MTime 所有生成文件的修改时间
var MTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type File struct {
	Path string
	Size int64
}

type Spec struct {
	Name        string
	Description string
	layout      func(r *rand.Rand) []File
}

func (s Spec) Files() []File {
	files := s.layout(rand.New(rand.NewPCG(seed(s.Name), Version)))
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

var specs = []Spec{
	{
		Name:        "small",
		Description: "100,000 files of 1-16 KiB in 5,000 directories (3 levels)",
		layout: func(r *rand.Rand) []File {
			files := make([]File, 0, 100_000)
			for i := range 100_000 {
				leaf := i / 20
				dir := fmt.Sprintf("d%d/d%d/d%02d", leaf/500, leaf/50%10, leaf%50)
				files = append(files, File{
					Path: fmt.Sprintf("%s/f%05d.bin", dir, i),
					Size: 1024 + r.Int64N(16384-1024+1),
				})
			}
			return files
		},
	},
	{
		Name:        "mixed",
		Description: "20,000 files, log-normal sizes (median 16 KiB, max 64 MiB) in 1,200 directories of varying depth",
		layout: func(r *rand.Rand) []File {
			dirs := []string{"."}
			for k := 1; k < 1200; k++ {
				parent := dirs[r.IntN(len(dirs))]
				for strings.Count(parent, "/") >= 5 {
					parent = dirs[r.IntN(len(dirs))]
				}
				dirs = append(dirs, filepath.ToSlash(filepath.Join(parent, fmt.Sprintf("dir%04d", k))))
			}
			files := make([]File, 0, 20_000)
			for i := range 20_000 {
				var size int64
				if r.IntN(100) > 0 {
					size = int64(math.Exp(math.Log(16384) + 1.8*r.NormFloat64()))
					size = min(size, 64<<20)
				}
				files = append(files, File{
					Path: filepath.ToSlash(filepath.Join(dirs[r.IntN(len(dirs))], fmt.Sprintf("file%05d.bin", i))),
					Size: size,
				})
			}
			return files
		},
	},
	{
		Name:        "large",
		Description: "4 files of 1 GiB",
		layout: func(*rand.Rand) []File {
			files := make([]File, 4)
			for i := range files {
				files[i] = File{Path: fmt.Sprintf("large%d.bin", i), Size: 1 << 30}
			}
			return files
		},
	},
}

func All() []Spec { return specs }

func Get(name string) (Spec, error) {
	for _, s := range specs {
		if s.Name == name {
			return s, nil
		}
	}
	return Spec{}, fmt.Errorf("unknown dataset %q", name)
}

func seed(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	return binary.LittleEndian.Uint64(sum[:8])
}

// Content 返回由 key 决定的 size 字节伪随机内容（不可压缩）
func Content(key string, size int64) io.Reader {
	return io.LimitReader(rand.NewChaCha8(sha256.Sum256([]byte(key))), size)
}

// WriteFile 写入 key 决定的内容，返回 sha256
func WriteFile(path, key string, size int64) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	if _, err := io.Copy(w, Content(key, size)); err != nil {
		f.Close()
		return "", err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type Entry struct {
	File
	SHA256 string
}

// Generate 在 dir 下生成数据集并写 manifest；manifest 已存在且版本一致时跳过
func Generate(s Spec, dir string) ([]Entry, error) {
	if m, err := ReadManifest(ManifestPath(dir)); err == nil {
		return m, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	files := s.Files()
	entries := make([]Entry, len(files))
	for i, f := range files {
		sum, err := WriteFile(filepath.Join(dir, filepath.FromSlash(f.Path)), s.Name+"/"+f.Path, f.Size)
		if err != nil {
			return nil, err
		}
		entries[i] = Entry{File: f, SHA256: sum}
	}
	if err := stampTimes(dir); err != nil {
		return nil, err
	}
	return entries, writeManifest(ManifestPath(dir), entries)
}

func stampTimes(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return os.Chtimes(p, MTime, MTime)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chtimes(dirs[i], MTime, MTime); err != nil {
			return err
		}
	}
	return nil
}

func ManifestPath(dir string) string { return filepath.Clean(dir) + ".manifest" }

func writeManifest(path string, entries []Entry) error {
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "# local-mirror bench dataset v%d\n", Version)
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%d\t%s\n", e.Path, e.Size, e.SHA256)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func ReadManifest(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] != fmt.Sprintf("# local-mirror bench dataset v%d", Version) {
		return nil, fmt.Errorf("%s: manifest version mismatch", path)
	}
	entries := make([]Entry, 0, len(lines)-1)
	for _, l := range lines[1:] {
		parts := strings.Split(l, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s: bad line %q", path, l)
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{File: File{Path: parts[0], Size: size}, SHA256: parts[2]})
	}
	return entries, nil
}

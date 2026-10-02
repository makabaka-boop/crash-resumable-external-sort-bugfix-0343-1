package ndsort

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// copyContext 在 io.Copy 的基础上周期性检查取消信号。ctx 为 nil 时不检查。
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	var ticks int
	for {
		if ctx != nil {
			if ticks%64 == 0 {
				if err := ctx.Err(); err != nil {
					return total, err
				}
			}
			ticks++
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				return total, errors.New("无效的写入返回值")
			}
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nr != nw {
				return total, io.ErrShortWrite
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return total, nil
			}
			return total, er
		}
	}
}

// atomicWriteFile 通过同目录临时文件 + fsync + rename 原子替换目标文件，
// 并对目录执行 fsync，保证崩溃后要么是旧内容、要么是新内容。
func atomicWriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(randBytes[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir 对目录执行 fsync（保证 rename 持久化）。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// hashInput 计算输入文件的大小与 SHA256，支持取消。
func hashInput(ctx context.Context, path string) (InputInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return InputInfo{}, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := copyContext(ctx, h, f)
	if err != nil {
		return InputInfo{}, err
	}
	return InputInfo{
		Path:   path,
		Size:   size,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// ParseSize 解析内存大小（如 32MiB、64KB、1048576）。
func ParseSize(s string) (int64, error) {
	return parseSize(s)
}

// parseSize 解析内存大小（如 32MiB、64KB、1048576）。
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("空的大小参数")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"MiB", 1 << 20}, {"Mi", 1 << 20}, {"MB", 1e6}, {"M", 1 << 20},
		{"KiB", 1 << 10}, {"Ki", 1 << 10}, {"KB", 1e3}, {"K", 1 << 10},
		{"GiB", 1 << 30}, {"Gi", 1 << 30}, {"GB", 1e9}, {"G", 1 << 30},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("无法解析大小 %q: %w", s, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("大小不能为负: %q", s)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小 %q", s)
	}
	return n, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) }

func readRand(b []byte) (int, error) { return rand.Read(b) }

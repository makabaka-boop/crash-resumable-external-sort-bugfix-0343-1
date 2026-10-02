package ndsort

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ManifestVersion 随排序语义/文件格式变化而递增。
// 旧版本清单一律不复用。
const ManifestVersion = 2

const manifestName = "manifest.json"

// InputInfo 标识某次排序的输入。只要字节内容变化，SHA256 必然不同，
// 旧清单立刻失效；Size 用于廉价的预检。
type InputInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SegmentInfo 描述一个已完成并校验过的有序临时段。
type SegmentInfo struct {
	File      string `json:"file"`
	Records   int64  `json:"records"`
	StartLine int64  `json:"start_line"`
	EndLine   int64  `json:"end_line"`
	SHA256    string `json:"sha256"` // 段文件自身哈希（清单级校验信息）
}

// RunInfo 描述一次归并产生的中间段。
type RunInfo struct {
	Level   int      `json:"level"`
	Index   int      `json:"index"`
	File    string   `json:"file"`
	Records int64    `json:"records"`
	Source  []string `json:"source"` // 输入段/上一级归并段的文件名
}

// OutputInfo 记录已成功发布的输出。
type OutputInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Records int64  `json:"records"`
	SHA256  string `json:"sha256"`
}

// Manifest 是断点续跑的全部依据。
type Manifest struct {
	Version        int           `json:"version"`
	CreatedAt      int64         `json:"created_at_unixnano"`
	UpdatedAt      int64         `json:"updated_at_unixnano"`
	Input          InputInfo     `json:"input"`
	FlushThreshold int           `json:"flush_threshold"`
	MaxMergeWay    int           `json:"max_merge_way"`
	RegionLimit    int           `json:"region_limit,omitempty"`
	Segments       []SegmentInfo `json:"segments"`
	Runs           []RunInfo     `json:"runs"`
	Output         *OutputInfo   `json:"output,omitempty"`
}

func manifestPath(workDir string) string { return filepath.Join(workDir, manifestName) }

func loadManifest(workDir string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath(workDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("清单损坏: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("清单版本 %d 不受支持（当前 %d）", m.Version, ManifestVersion)
	}
	return &m, nil
}

// save 原子写入清单：临时文件 + fsync + rename + 目录 fsync。
func (m *Manifest) save(workDir string) error {
	m.UpdatedAt = time.Now().UnixNano()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(manifestPath(workDir), append(data, '\n'), 0o600)
}

// matchesInput 判断输入标识是否与清单一致。
func (m *Manifest) matchesInput(in InputInfo) bool {
	return m.Input.Size == in.Size && m.Input.SHA256 == in.SHA256
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyContext(nil, h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

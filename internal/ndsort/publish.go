package ndsort

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// publish 将最终有序段流式写为输出临时文件，校验后原子替换旧输出。
// 改名之前的任何失败都只影响临时文件，旧输出原封不动。
//
// 若设置了每 region 上限，截断闸门在这一条全局有序流上生效；返回值是
// 实际写入输出（截断后）的记录数。
func (e *engine) publish(sources []artifactRef, totalRecords int64) (int64, error) {
	outPath := e.opts.OutputPath
	if err := e.ctx.Err(); err != nil {
		return 0, err
	}

	// 与输入同路径已在 newEngine 中禁止；此处再确认工作目录不在输出路径上。
	tmp, err := createOutputTemp(outPath)
	if err != nil {
		return 0, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			tmp.remove()
		}
	}()

	h := sha256.New()
	var written int64

	gate := newRegionGate(e.opts.RegionLimit)
	if len(sources) == 1 {
		n, err := e.copyArtifact(sources[0].file, tmp.f, h, gate)
		if err != nil {
			return 0, err
		}
		written = n
	}
	// sources 为空 => 空输入 => 写出空文件，written 保持 0。

	// 截断只会丢弃记录：实际写入不得多于全量条数；不截断时必须逐条吻合。
	if written > totalRecords {
		return 0, fmt.Errorf("输出记录数不匹配: 全量 %d 实际 %d", totalRecords, written)
	}
	if e.opts.RegionLimit <= 0 && written != totalRecords {
		return 0, fmt.Errorf("输出记录数不匹配: 期望 %d 实际 %d", totalRecords, written)
	}
	if err := tmp.f.Sync(); err != nil {
		return 0, fmt.Errorf("输出刷盘失败: %w", err)
	}
	fi, err := tmp.f.Stat()
	if err != nil {
		return 0, err
	}
	if err := tmp.f.Close(); err != nil {
		return 0, fmt.Errorf("关闭输出临时文件失败: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	// 钩子：输出已完整写入临时文件但尚未改名。
	if e.hooks.AfterOutputWritten != nil {
		if err := e.hooks.AfterOutputWritten(e.ctx, tmp.path, written); err != nil {
			return 0, err
		}
	}

	// 改名前再独立校验一次临时文件（捕获钩子篡改或磁盘异常）。
	gotSum, gotSize, err := sha256File(tmp.path)
	if err != nil {
		return 0, fmt.Errorf("回读输出失败: %w", err)
	}
	if gotSum != sum || gotSize != fi.Size() {
		return 0, &ChecksumError{Path: tmp.path, Err: errors.New("临时输出回读校验不匹配")}
	}

	// 先登记“已发布”意图，再执行不可分割的替换。
	e.manifest.Output = &OutputInfo{
		Path:    outPath,
		Size:    fi.Size(),
		Records: written,
		SHA256:  sum,
	}
	if err := e.manifest.save(e.opts.WorkDir); err != nil {
		return 0, err
	}

	if err := os.Rename(tmp.path, outPath); err != nil {
		// 改名失败：清掉输出登记并保存，保证状态与磁盘一致。
		e.manifest.Output = nil
		_ = e.manifest.save(e.opts.WorkDir)
		return 0, fmt.Errorf("替换输出失败: %w", err)
	}
	if err := syncDir(filepath.Dir(outPath)); err != nil {
		return 0, err
	}
	cleanup = false

	// 发布成功：清理段数据，仅保留记录已发布输出的清单，
	// 使重复执行可以廉价地确认“无需重做”。
	if err := e.pruneArtifacts(); err != nil {
		// 清理失败不影响已成功的发布。
		return written, nil
	}
	return written, nil
}

// copyArtifact 把有序段中的原始 JSON 行流式写入输出（每行追加 '\n'），
// 同时计算输出的 SHA256。记录先经过 region 闸门截断；返回实际写出的记录数。
func (e *engine) copyArtifact(name string, w io.Writer, h hash.Hash, gate *regionGate) (int64, error) {
	seg, err := openSegment(filepath.Join(e.opts.WorkDir, name))
	if err != nil {
		return 0, err
	}
	defer seg.Close()

	mw := io.MultiWriter(w, h)
	var n int64
	ticks := 0
	for {
		if ticks%512 == 0 {
			if err := e.ctx.Err(); err != nil {
				return n, err
			}
		}
		ticks++
		rec, err := seg.Next()
		if err != nil {
			if isEOF(err) {
				return n, nil
			}
			return n, err
		}
		if !gate.keep(rec) {
			continue
		}
		if _, err := io.WriteString(mw, rec.Raw); err != nil {
			return n, fmt.Errorf("写入输出失败: %w", err)
		}
		if _, err := mw.Write([]byte{'\n'}); err != nil {
			return n, fmt.Errorf("写入输出失败: %w", err)
		}
		n++
	}
}

// pruneArtifacts 删除全部段与归并段文件并更新清单。
func (e *engine) pruneArtifacts() error {
	names := make([]string, 0, len(e.manifest.Segments)+len(e.manifest.Runs))
	for _, s := range e.manifest.Segments {
		names = append(names, s.File)
	}
	for _, r := range e.manifest.Runs {
		names = append(names, r.File)
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(e.opts.WorkDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	e.manifest.Segments = []SegmentInfo{}
	e.manifest.Runs = []RunInfo{}
	return e.manifest.save(e.opts.WorkDir)
}

// outputTemp 是输出侧的临时文件句柄。
type outputTemp struct {
	f    *os.File
	path string
}

func createOutputTemp(outPath string) (*outputTemp, error) {
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var rb [8]byte
	if _, err := readRand(rb[:]); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "."+filepath.Base(outPath)+".out-tmp-"+hex.EncodeToString(rb[:]))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return &outputTemp{f: f, path: path}, nil
}

func (t *outputTemp) remove() {
	t.f.Close()
	_ = os.Remove(t.path)
}

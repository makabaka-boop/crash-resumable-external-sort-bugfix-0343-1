package ndsort

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// buildSegments 读取输入，在内存中积累记录，达到 flushThreshold 后
// 排序并落盘。已完成的段记录在清单中，中断重跑时从断点继续。
func (e *engine) buildSegments(inputSize int64) error {
	f, err := os.Open(e.opts.InputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// 快速一致性预检：大小不匹配意味着哈希之后文件被替换。
	if fi, err := f.Stat(); err != nil {
		return err
	} else if fi.Size() != inputSize {
		return fmt.Errorf("输入文件在校验后发生变化")
	}

	lr := newLineReader(f)

	// 跳过清单中已完成段覆盖的行。段按顺序生成，覆盖连续行区间。
	resumeLines := int64(0)
	if n := len(e.manifest.Segments); n > 0 {
		resumeLines = e.manifest.Segments[n-1].EndLine
	}
	if err := skipLines(lr, resumeLines); err != nil {
		return err
	}

	var (
		buf      []*Record
		bufBytes int
		segStart = resumeLines + 1
		lastLine = resumeLines
	)

	for {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		line, lineNo, err := lr.next()
		if err != nil {
			if isEOF(err) && lineNo == lastLine {
				break // 输入耗尽，且本次没有新数据
			}
			return err
		}
		lastLine = lineNo
		rec, perr := ParseRecord(line, lineNo)
		if perr != nil {
			return perr
		}

		buf = append(buf, rec)
		bufBytes += rec.EstimateBytes()

		if bufBytes >= e.flushThreshold {
			if err := e.flushSegment(buf, segStart, lineNo); err != nil {
				return err
			}
			e.stats.BuiltSegments++
			buf = nil
			bufBytes = 0
			segStart = lineNo + 1
		}
	}

	if len(buf) > 0 {
		if err := e.flushSegment(buf, segStart, lastLine); err != nil {
			return err
		}
		e.stats.BuiltSegments++
	}
	return nil
}

// flushSegment 对驻留记录排序、写段、校验、登记清单，然后触发边界钩子。
func (e *engine) flushSegment(buf []*Record, startLine, endLine int64) error {
	// 稳定排序；键中含行号，相同业务键按输入顺序排列。
	sort.SliceStable(buf, func(i, j int) bool {
		return CompareKeys(buf[i], buf[j]) < 0
	})

	// 排序后、刷盘前：这是单段驻留内存最大的时刻（测试内存上限用）。
	if e.hooks.BeforeSegmentFlush != nil {
		if err := e.hooks.BeforeSegmentFlush(e.ctx, len(buf)); err != nil {
			return err
		}
	}

	seq := len(e.manifest.Segments) // 续跑时自然接在已有段之后
	name := segmentName(seq)
	path := filepath.Join(e.opts.WorkDir, name)
	sw, err := createSegment(path)
	if err != nil {
		return fmt.Errorf("创建临时段失败: %w", err)
	}
	for _, rec := range buf {
		if err := sw.append(rec); err != nil {
			sw.abort()
			return fmt.Errorf("写入临时段失败: %w", err)
		}
		if err := e.ctx.Err(); err != nil {
			sw.abort()
			return err
		}
	}
	if err := sw.close(); err != nil {
		return fmt.Errorf("关闭临时段失败: %w", err)
	}

	// 立即重新打开并完整校验，杜绝把坏段写进清单。
	seg, err := openSegment(path)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	count := seg.Count()
	if err := seg.Close(); err != nil {
		return err
	}
	sum, _, err := sha256File(path)
	if err != nil {
		return err
	}

	e.manifest.Segments = append(e.manifest.Segments, SegmentInfo{
		File:      name,
		Records:   count,
		StartLine: startLine,
		EndLine:   endLine,
		SHA256:    sum,
	})
	if err := e.manifest.save(e.opts.WorkDir); err != nil {
		return fmt.Errorf("写入清单失败: %w", err)
	}

	var est int64
	for _, r := range buf {
		est += int64(r.EstimateBytes())
	}
	if est > e.stats.SegmentMaxBytes {
		e.stats.SegmentMaxBytes = est
	}

	if e.hooks.AfterSegmentSaved != nil {
		if err := e.hooks.AfterSegmentSaved(e.ctx, int64(seq), len(e.manifest.Segments)); err != nil {
			return err
		}
	}
	return nil
}

func totalSegmentRecords(m *Manifest) int64 {
	var n int64
	for _, s := range m.Segments {
		n += s.Records
	}
	return n
}

// skipLines 丢弃前 n 行。
func skipLines(lr *lineReader, n int64) error {
	for i := int64(0); i < n; i++ {
		if _, _, err := lr.next(); err != nil {
			return err
		}
	}
	return nil
}

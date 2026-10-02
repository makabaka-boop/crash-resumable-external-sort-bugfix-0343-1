package ndsort

import (
	"container/heap"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// artifactRef 引用一个有序段（初始临时段或归并中间段）。
type artifactRef struct {
	file    string
	records int64
}

// mergeAll 将全部初始段按 MaxMergeWay 路逐级归并，返回最终唯一有序段
// 及其总记录数。没有输入时返回空列表。
func (e *engine) mergeAll() ([]artifactRef, int64, error) {
	sources := make([]artifactRef, 0, len(e.manifest.Segments))
	var total int64
	for _, s := range e.manifest.Segments {
		sources = append(sources, artifactRef{file: s.File, records: s.Records})
		total += s.Records
	}
	if len(sources) <= 1 {
		e.stats.MergeRuns = 0
		return sources, total, nil
	}

	level := 0
	for len(sources) > 1 {
		var next []artifactRef
		for idx := 0; idx < len(sources); idx += e.opts.MaxMergeWay {
			end := idx + e.opts.MaxMergeWay
			if end > len(sources) {
				end = len(sources)
			}
			group := sources[idx:end]
			groupIdx := idx / e.opts.MaxMergeWay

			// 单组无需归并（只会是最后仅剩一个的情况，理论上循环已结束；
			// 保留处理以覆盖路数边界）。
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}

			ref, reused, err := e.ensureRun(level, groupIdx, group)
			if err != nil {
				return nil, 0, err
			}
			if reused {
				e.stats.ReusedRuns++
			} else {
				e.stats.BuiltRuns++
			}
			next = append(next, ref)
		}
		sources = next
		level++
	}
	e.stats.MergeRuns = e.stats.ReusedRuns + e.stats.BuiltRuns
	return sources, total, nil
}

// ensureRun 返回 (level,index) 处与 group 完全一致的归并段；
// 清单中已有且来源匹配则复用，否则丢弃该点之后的全部旧归并段后重建。
func (e *engine) ensureRun(level, index int, group []artifactRef) (artifactRef, bool, error) {
	wantSources := make([]string, len(group))
	var wantRecords int64
	for i, g := range group {
		wantSources[i] = g.file
		wantRecords += g.records
	}

	pos := e.findRun(level, index)
	if pos >= 0 {
		r := e.manifest.Runs[pos]
		if r.Records == wantRecords && stringsEqual(r.Source, wantSources) {
			return artifactRef{file: r.File, records: r.Records}, true, nil
		}
		// 来源不一致（通常因参数变化，但那种情况整体已重置；此处防御性处理）。
		if err := e.dropRunsFrom(pos); err != nil {
			return artifactRef{}, false, err
		}
	}
	ref, err := e.buildRun(level, index, group, wantRecords)
	if err != nil {
		return artifactRef{}, false, err
	}
	return ref, false, nil
}

func (e *engine) findRun(level, index int) int {
	for i, r := range e.manifest.Runs {
		if r.Level == level && r.Index == index {
			return i
		}
	}
	return -1
}

// dropRunsFrom 删除排序列表中 pos 及其后的归并段。
func (e *engine) dropRunsFrom(pos int) error {
	for _, r := range e.manifest.Runs[pos:] {
		_ = os.Remove(filepath.Join(e.opts.WorkDir, r.File))
	}
	e.manifest.Runs = e.manifest.Runs[:pos]
	return e.manifest.save(e.opts.WorkDir)
}

// mergeCursor 是堆中的一个归并游标。
type mergeCursor struct {
	rec *Record
	src int
	seg *Segment
}

type mergeHeap []mergeCursor

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	return CompareKeys(h[i].rec, h[j].rec) < 0
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeCursor)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// buildRun 执行一次 k 路归并并生成新的归并段。
func (e *engine) buildRun(level, index int, group []artifactRef, wantRecords int64) (artifactRef, error) {
	name := runName(level, index)
	path := filepath.Join(e.opts.WorkDir, name)

	segs := make([]*Segment, 0, len(group))
	defer func() {
		for _, s := range segs {
			s.Close()
		}
	}()

	h := &mergeHeap{}
	heap.Init(h)
	for i, g := range group {
		if err := e.ctx.Err(); err != nil {
			return artifactRef{}, err
		}
		seg, err := openSegment(filepath.Join(e.opts.WorkDir, g.file))
		if err != nil {
			return artifactRef{}, err
		}
		segs = append(segs, seg)
		rec, err := seg.Next()
		if err != nil && !isEOF(err) {
			return artifactRef{}, err
		}
		if rec != nil {
			heap.Push(h, mergeCursor{rec: rec, src: i, seg: seg})
		}
	}

	sw, err := createSegment(path)
	if err != nil {
		return artifactRef{}, fmt.Errorf("创建归并段失败: %w", err)
	}
	var written int64
	ticks := 0
	for h.Len() > 0 {
		if ticks%1024 == 0 {
			if err := e.ctx.Err(); err != nil {
				sw.abort()
				return artifactRef{}, err
			}
		}
		ticks++
		cur := heap.Pop(h).(mergeCursor)
		if err := sw.append(cur.rec); err != nil {
			sw.abort()
			return artifactRef{}, fmt.Errorf("写入归并段失败: %w", err)
		}
		written++
		nxt, err := cur.seg.Next()
		if err != nil && !isEOF(err) {
			sw.abort()
			return artifactRef{}, err
		}
		if nxt != nil {
			heap.Push(h, mergeCursor{rec: nxt, src: cur.src, seg: cur.seg})
		}
	}
	if err := sw.close(); err != nil {
		return artifactRef{}, fmt.Errorf("关闭归并段失败: %w", err)
	}
	if written != wantRecords {
		_ = os.Remove(path)
		return artifactRef{}, fmt.Errorf("归并段 %s 记录数不匹配: 期望 %d 实际 %d", name, wantRecords, written)
	}

	// 完成后立即完整校验。
	check, err := openSegment(path)
	if err != nil {
		_ = os.Remove(path)
		return artifactRef{}, err
	}
	count := check.Count()
	if err := check.Close(); err != nil {
		return artifactRef{}, err
	}

	e.manifest.Runs = append(e.manifest.Runs, RunInfo{
		Level:   level,
		Index:   index,
		File:    name,
		Records: count,
		Source:  wantSourcesOf(group),
	})
	sortRuns(e.manifest.Runs)
	if err := e.manifest.save(e.opts.WorkDir); err != nil {
		return artifactRef{}, err
	}

	if e.hooks.AfterMergeRun != nil {
		if err := e.hooks.AfterMergeRun(e.ctx, level, index, len(e.manifest.Runs)); err != nil {
			return artifactRef{}, err
		}
	}
	return artifactRef{file: name, records: count}, nil
}

func wantSourcesOf(group []artifactRef) []string {
	out := make([]string, len(group))
	for i, g := range group {
		out[i] = g.file
	}
	return out
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortRuns 保持归并段清单按 (level, index) 排列。
func sortRuns(runs []RunInfo) {
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Level != runs[j].Level {
			return runs[i].Level < runs[j].Level
		}
		return runs[i].Index < runs[j].Index
	})
}

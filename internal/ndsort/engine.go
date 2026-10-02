package ndsort

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 默认参数。
const (
	DefaultMaxSortBytes int64 = 32 << 20 // 单次排序可用内存上限 32 MiB
	DefaultMaxMergeWay        = 16       // 最多 16 路归并
	minMaxSortBytes     int64 = 64 << 10
)

// Options 控制一次排序任务。
type Options struct {
	InputPath    string
	OutputPath   string
	WorkDir      string // 留空时在输出文件旁创建隐藏工作目录
	MaxSortBytes int64  // 单次排序内存上限；达到约 3/4 时刷段
	MaxMergeWay  int    // 归并路数上限（≤16）
	RegionLimit  int    // 每个 region 的输出条数上限；0 表示全部输出
}

// Hooks 是供测试注入中断/失败的边界回调。回调返回非 nil 错误时
// 引擎中止运行；已经提交（清单已记录）的成果会被保留。
type Hooks struct {
	// 一个临时段完成内存排序、即将刷盘之前调用（驻留内存最大时刻）。
	BeforeSegmentFlush func(ctx context.Context, records int) error
	// 一个临时段排序、落盘、校验并写入清单之后调用。
	AfterSegmentSaved func(ctx context.Context, seq int64, totalSegments int) error
	// 一次归并运行完成并写入清单之后调用。
	AfterMergeRun func(ctx context.Context, level, index, totalRuns int) error
	// 最终输出写入临时文件（尚未改名）之后调用；
	// tmpPath 即临时文件路径，测试可在此截断/篡改它或返回错误。
	AfterOutputWritten func(ctx context.Context, tmpPath string, records int64) error
}

// Stats 汇报本次执行的结果。
type Stats struct {
	TotalRecords    int64
	Segments        int
	SegmentMaxBytes int64 // 刷段前驻留记录负载估算的峰值
	MergeRuns       int
	ReusedSegments  int // 从清单复用的段数
	BuiltSegments   int // 本次新建的段数
	ReusedRuns      int
	BuiltRuns       int
	Resumed         bool // 是否存在被复用的历史成果
	ReusedOutput    bool // 是否直接复用了已发布的完整输出
}

// Run 执行（或继续）一次外部排序。失败时绝不部分发布：
// 解析错误、磁盘错误、取消都会以非 nil error 返回，旧输出保持不变。
func Run(ctx context.Context, opts Options, hooks Hooks) (*Stats, error) {
	e, err := newEngine(ctx, opts, hooks)
	if err != nil {
		return nil, err
	}
	e.ctx = ctx

	// 输入标识：字节变化（大小/SHA256 任一不同）即令旧清单作废。
	// 使用 newEngine 归一化后的绝对路径，保证清单内外路径一致。
	in, err := hashInput(ctx, e.opts.InputPath)
	if err != nil {
		return nil, fmt.Errorf("读取输入失败: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(e.opts.OutputPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}

	m, err := loadManifest(e.opts.WorkDir)
	if err != nil {
		// 清单本身不可读：无法信任任何旧产物，整体重来。
		_ = resetWorkDir(e.opts.WorkDir)
		m = nil
	}
	if m != nil && (!m.matchesInput(in) || m.FlushThreshold != e.flushThreshold || m.MaxMergeWay != e.opts.MaxMergeWay || m.Input.Path != e.opts.InputPath) {
		if err := resetWorkDir(e.opts.WorkDir); err != nil {
			return nil, err
		}
		m = nil
	}

	if m != nil && m.Output != nil {
		// 已发布输出且校验通过：直接复用，什么都不重做。
		if e.verifyPublished(m.Output) {
			e.stats.Resumed = true
			e.stats.ReusedOutput = true
			e.stats.TotalRecords = m.Output.Records
			return &e.stats, nil
		}
		// 输出已丢失/损坏：若临时段也已被清理，则只能全量重来。
		if len(m.Segments) == 0 {
			if err := resetWorkDir(e.opts.WorkDir); err != nil {
				return nil, err
			}
			m = nil
		} else {
			m.Output = nil
			_ = m.save(e.opts.WorkDir)
		}
	}

	if m == nil {
		m = &Manifest{
			Version:        ManifestVersion,
			CreatedAt:      nowUnixNano(),
			Input:          in,
			FlushThreshold: e.flushThreshold,
			MaxMergeWay:    e.opts.MaxMergeWay,
			RegionLimit:    e.opts.RegionLimit,
			Segments:       []SegmentInfo{},
			Runs:           []RunInfo{},
		}
		if err := os.MkdirAll(e.opts.WorkDir, 0o700); err != nil {
			return nil, err
		}
		if err := m.save(e.opts.WorkDir); err != nil {
			return nil, err
		}
	}
	e.manifest = m

	if len(m.Segments) > 0 || len(m.Runs) > 0 {
		e.stats.Resumed = true
	}

	// 校验清单中引用的全部产物，损坏则丢弃其后的成果。
	if err := e.verifyAndTrim(); err != nil {
		return nil, err
	}
	if err := e.cleanupOrphans(); err != nil {
		return nil, err
	}

	if err := e.buildSegments(in.Size); err != nil {
		return nil, err
	}

	sources, totalRecords, err := e.mergeAll()
	if err != nil {
		return nil, err
	}
	e.stats.TotalRecords = totalRecords

	if err := e.publish(sources, totalRecords); err != nil {
		return nil, err
	}
	return &e.stats, nil
}

type engine struct {
	opts           Options
	hooks          Hooks
	ctx            context.Context
	manifest       *Manifest
	flushThreshold int
	stats          Stats
}

func newEngine(ctx context.Context, opts Options, hooks Hooks) (*engine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.InputPath == "" || opts.OutputPath == "" {
		return nil, errors.New("必须指定输入和输出路径")
	}
	inAbs, err := filepath.Abs(opts.InputPath)
	if err != nil {
		return nil, err
	}
	outAbs, err := filepath.Abs(opts.OutputPath)
	if err != nil {
		return nil, err
	}
	if inAbs == outAbs {
		return nil, errors.New("输入与输出不能是同一个文件")
	}
	opts.InputPath, opts.OutputPath = inAbs, outAbs

	if opts.MaxSortBytes == 0 {
		opts.MaxSortBytes = DefaultMaxSortBytes
	}
	if opts.MaxSortBytes < minMaxSortBytes {
		return nil, fmt.Errorf("排序内存上限过小（至少 %d 字节）", minMaxSortBytes)
	}
	if opts.MaxMergeWay == 0 {
		opts.MaxMergeWay = DefaultMaxMergeWay
	}
	if opts.MaxMergeWay < 2 || opts.MaxMergeWay > DefaultMaxMergeWay {
		return nil, fmt.Errorf("归并路数必须在 2..%d 之间", DefaultMaxMergeWay)
	}
	if opts.RegionLimit < 0 {
		return nil, errors.New("每个 region 的输出上限不能为负")
	}
	if opts.WorkDir == "" {
		opts.WorkDir = filepath.Join(filepath.Dir(outAbs), "."+filepath.Base(outAbs)+".ndsort-work")
	} else {
		wd, err := filepath.Abs(opts.WorkDir)
		if err != nil {
			return nil, err
		}
		opts.WorkDir = wd
	}

	// 驻留记录达到排序内存的约 3/4 即刷段，留出排序临时开销与运行时余量，
	// 保证单次排序峰值明显低于上限。
	ft := int(opts.MaxSortBytes * 3 / 4)
	return &engine{opts: opts, hooks: hooks, flushThreshold: ft}, nil
}

// verifyPublished 校验已发布输出的路径、大小与 SHA256。
func (e *engine) verifyPublished(o *OutputInfo) bool {
	if o.Path != e.opts.OutputPath {
		return false
	}
	h, size, err := sha256File(o.Path)
	if err != nil {
		return false
	}
	return size == o.Size && h == o.SHA256
}

// verifyAndTrim 逐个校验清单中的段与归并段；任一损坏即丢弃它及其下游。
func (e *engine) verifyAndTrim() error {
	m := e.manifest
	goodSegs := m.Segments[:0]
	badSeg := -1
	for i, s := range m.Segments {
		if err := verifyArtifact(e.opts.WorkDir, s.File, s.SHA256); err != nil {
			badSeg = i
			break
		}
		goodSegs = append(goodSegs, s)
	}
	if badSeg >= 0 {
		for _, s := range m.Segments[badSeg:] {
			_ = os.Remove(filepath.Join(e.opts.WorkDir, s.File))
		}
		m.Segments = goodSegs
		m.Runs = []RunInfo{} // 段失效，所有归并段都失去依据
		if err := m.save(e.opts.WorkDir); err != nil {
			return err
		}
	}
	e.stats.ReusedSegments = len(m.Segments)

	// 归并段按 (level, index) 排列。
	sort.SliceStable(m.Runs, func(i, j int) bool {
		if m.Runs[i].Level != m.Runs[j].Level {
			return m.Runs[i].Level < m.Runs[j].Level
		}
		return m.Runs[i].Index < m.Runs[j].Index
	})

	goodRuns := m.Runs[:0]
	badRun := -1
	for i, r := range m.Runs {
		if err := verifyArtifact(e.opts.WorkDir, r.File, ""); err != nil {
			badRun = i
			break
		}
		goodRuns = append(goodRuns, r)
	}
	if badRun >= 0 {
		for _, r := range m.Runs[badRun:] {
			_ = os.Remove(filepath.Join(e.opts.WorkDir, r.File))
		}
		m.Runs = goodRuns
		if err := m.save(e.opts.WorkDir); err != nil {
			return err
		}
	}
	e.stats.ReusedRuns = len(m.Runs)
	return nil
}

// verifyArtifact 校验工作目录中的段文件：清单哈希（若记录）+ 完整帧校验。
func verifyArtifact(workDir, name, wantSHA string) error {
	path := filepath.Join(workDir, name)
	if wantSHA != "" {
		got, _, err := sha256File(path)
		if err != nil {
			return err
		}
		if got != wantSHA {
			return &ChecksumError{Path: path, Err: errors.New("清单记录的 sha256 不匹配")}
		}
	}
	s, err := openSegment(path)
	if err != nil {
		return err
	}
	return s.Close()
}

// cleanupOrphans 删除工作目录中未被清单引用的遗留段文件（崩溃残留）。
func (e *engine) cleanupOrphans() error {
	referenced := map[string]bool{manifestName: true}
	for _, s := range e.manifest.Segments {
		referenced[s.File] = true
	}
	for _, r := range e.manifest.Runs {
		referenced[r.File] = true
	}
	entries, err := os.ReadDir(e.opts.WorkDir)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() {
			continue
		}
		if isArtifactName(name) && !referenced[name] {
			if err := os.Remove(filepath.Join(e.opts.WorkDir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// resetWorkDir 清空工作目录中的所有旧产物。
func resetWorkDir(workDir string) error {
	entries, err := os.ReadDir(workDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if err := os.RemoveAll(filepath.Join(workDir, ent.Name())); err != nil {
			return err
		}
	}
	return nil
}

func segmentName(seq int) string { return fmt.Sprintf("segment-%06d.dat", seq) }

func runName(level, index int) string {
	return fmt.Sprintf("run-%02d-%06d.dat", level, index)
}

func isArtifactName(name string) bool {
	switch {
	case len(name) >= 8 && name[:8] == "segment-":
		return filepath.Ext(name) == ".dat"
	case len(name) >= 4 && name[:4] == "run-":
		return filepath.Ext(name) == ".dat"
	}
	return false
}

func nowUnixNano() int64 {
	return time.Now().UnixNano()
}

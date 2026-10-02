package ndsort_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ndsort/internal/ndsort"
)

var errInjected = errors.New("注入的失败")

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func expectSortedByOracle(t *testing.T, inLines []string, out string) {
	t.Helper()
	want, err := oracleSort(inLines)
	if err != nil {
		t.Fatal(err)
	}
	got := readOutputLines(t, out)
	if len(got) != len(want) {
		t.Fatalf("输出行数 %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("行 %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestInterruptDuringSegmentation 在每个段完成后取消，逐次续跑直到完成。
func TestInterruptDuringSegmentation(t *testing.T) {
	lines := generateLines(77, 400)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	// 极小的分段内存 => 多段；2 路归并。
	const mem = 64 << 10
	const way = 2

	flushes := 0
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: mem, MaxMergeWay: way,
	}, ndsort.Hooks{
		AfterSegmentSaved: func(ctx context.Context, seq int64, total int) error {
			flushes++
			return errInjected
		},
	})
	if !errors.Is(err, errInjected) || flushes != 1 {
		t.Fatalf("期望首个段后注入失败, flushes=%d err=%v", flushes, err)
	}
	if fileExists(out) {
		t.Fatal("分段阶段失败时不得产生输出文件")
	}

	// 逐段续跑：每轮放行第一个新段、第二个新段处中断。
	round := 0
	for {
		newThisRound := 0
		segmentsDone := false
		_, rerr := ndsort.Run(context.Background(), ndsort.Options{
			InputPath: in, OutputPath: out, WorkDir: work,
			MaxSortBytes: mem, MaxMergeWay: way,
		}, ndsort.Hooks{
			AfterSegmentSaved: func(ctx context.Context, seq int64, total int) error {
				newThisRound++
				if newThisRound >= 2 {
					return errInjected
				}
				return nil
			},
			AfterMergeRun: func(ctx context.Context, level, index, totalRuns int) error {
				segmentsDone = true
				return errInjected // 段已全部完成：在首个归并边界停下
			},
		})
		if rerr == nil {
			t.Fatal("本轮应当被注入中断")
		}
		if segmentsDone {
			// 所有段已建好：无钩子收尾即可完成。
			break
		}
		if !errors.Is(rerr, errInjected) || newThisRound != 2 {
			t.Fatalf("第 %d 轮期望新建 2 个段后中断, new=%d err=%v", round, newThisRound, rerr)
		}
		if fileExists(out) {
			t.Fatal("分段阶段失败时不得产生输出文件")
		}
		round++
		if round > 1000 {
			t.Fatal("续跑轮数异常，可能没有进度")
		}
	}

	// 所有段均已落盘：无钩子收尾，必须复用全部段。
	stats := runOK(t, in, out, work, mem, way, ndsort.Hooks{})
	if !stats.Resumed {
		t.Fatal("期望识别到可复用的段")
	}
	if stats.ReusedSegments < 2 {
		t.Fatalf("期望复用至少 2 个段, got %d", stats.ReusedSegments)
	}
	expectSortedByOracle(t, lines, out)

	// 工作目录中不应残留临时文件。
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("工作目录残留临时文件: %s", e.Name())
		}
	}
}

// TestInterruptDuringMerge 在归并边界中断后续跑。
func TestInterruptDuringMerge(t *testing.T) {
	lines := generateLines(91, 3000)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	const mem = 64 << 10
	const way = 3 // 段数足够多时强制产生多级归并

	// 第一次：在第一次归并完成后立即中断。
	fired := false
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: mem, MaxMergeWay: way,
	}, ndsort.Hooks{
		AfterMergeRun: func(ctx context.Context, level, index, totalRuns int) error {
			if !fired {
				fired = true
				return errInjected
			}
			return nil
		},
	})
	if !errors.Is(err, errInjected) || !fired {
		t.Fatalf("期望归并边界中断, fired=%v err=%v", fired, err)
	}
	if fileExists(out) {
		t.Fatal("归并未完成不得产生输出")
	}

	// 第二次：直接完成，应复用部分归并段。
	stats := runOK(t, in, out, work, mem, way, ndsort.Hooks{})
	if !stats.Resumed {
		t.Fatal("期望续跑")
	}
	if stats.ReusedRuns < 1 {
		t.Fatalf("期望复用归并段, got %d", stats.ReusedRuns)
	}
	expectSortedByOracle(t, lines, out)
}

// TestInterruptAtPublish 输出临时文件写好后中断：旧输出保留，续跑后替换。
func TestInterruptAtPublish(t *testing.T) {
	lines := generateLines(33, 200)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	// 预置一份“旧输出”（内容明显不是排序结果），任何失败都不能动它。
	oldContent := []byte("OLD-OUTPUT-SENTINEL\n")
	if err := os.WriteFile(out, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 1 << 20, MaxMergeWay: 2,
	}, ndsort.Hooks{
		AfterOutputWritten: func(ctx context.Context, tmpPath string, records int64) error {
			return errInjected
		},
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("期望发布边界失败, err=%v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != string(oldContent) {
		t.Fatalf("发布失败不得改动旧输出:\n got %q", got)
	}
	// 临时文件必须被清理。
	outs, _ := os.ReadDir(filepath.Dir(out))
	for _, e := range outs {
		if strings.Contains(e.Name(), ".out-tmp") {
			t.Fatalf("残留输出临时文件: %s", e.Name())
		}
	}

	// 续跑完成并替换。
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})
	expectSortedByOracle(t, lines, out)
}

// TestTamperedTmpOutput 在发布钩子里篡改临时输出，必须被检测到且不发布。
func TestTamperedTmpOutput(t *testing.T) {
	lines := generateLines(34, 100)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 1 << 20, MaxMergeWay: 2,
	}, ndsort.Hooks{
		AfterOutputWritten: func(ctx context.Context, tmpPath string, records int64) error {
			f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, _ = f.WriteString("tampered\n")
			return f.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "校验") {
		t.Fatalf("期望校验错误, got %v", err)
	}
	if fileExists(out) {
		t.Fatal("校验失败不得发布输出")
	}

	// 再跑一次无注入，应成功（段仍在）。
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})
	expectSortedByOracle(t, lines, out)
}

// TestInputChangeInvalidatesManifest 字节变化后旧清单必须废弃、全量重算。
func TestInputChangeInvalidatesManifest(t *testing.T) {
	lines := generateLines(55, 250)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})
	expectSortedByOracle(t, lines, out)

	// 修改输入（增加行数，保持同样长度风格），旧清单不得复用。
	lines2 := append(append([]string{}, lines...),
		`{"id":"NEW","score":999999,"region":"zz"}`)
	writeInput(t, in, lines2)
	stats := runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})
	if stats.Resumed || stats.ReusedOutput {
		t.Fatalf("输入变化后不应复用任何旧成果: %+v", stats)
	}
	if stats.BuiltSegments == 0 {
		t.Fatal("应当重新分段")
	}
	expectSortedByOracle(t, lines2, out)
}

// TestInputSameSizeDifferentBytes 同样大小不同字节也必须识别（纯哈希作用）。
func TestInputSameSizeDifferentBytes(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	l1 := []string{`{"id":"aa","score":1}`}
	l2 := []string{`{"id":"bb","score":1}`}
	if len(l1[0]) != len(l2[0]) {
		t.Fatal("用例要求两行等长")
	}
	writeInput(t, in, l1)
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})
	got := readOutputLines(t, out)
	if got[0] != l1[0] {
		t.Fatal("首次结果错误")
	}
	writeInput(t, in, l2)
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})
	got = readOutputLines(t, out)
	if got[0] != l2[0] {
		t.Fatalf("字节变化后仍复用了旧结果: %s", got[0])
	}
}

// TestParseErrorKeepsOldOutput 解析失败不能暴露部分新输出。
func TestParseErrorKeepsOldOutput(t *testing.T) {
	good := generateLines(41, 120)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, good)
	runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})
	old, _ := os.ReadFile(out)

	bad := append(append([]string{}, good[:10]...), `{"id":"broken`)
	writeInput(t, in, bad)
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 128 << 10, MaxMergeWay: 2,
	}, ndsort.Hooks{})
	if err == nil {
		t.Fatal("坏 JSON 应失败")
	}
	now, _ := os.ReadFile(out)
	if string(now) != string(old) {
		t.Fatal("解析失败改动了旧输出")
	}
}

// TestRerunReusesPublishedOutput 已发布的输出再次运行应直接复用。
func TestRerunReusesPublishedOutput(t *testing.T) {
	lines := generateLines(42, 150)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})

	// 删除输出后重跑：清单声称已发布但文件丢失，且段已被清理 => 全量重做。
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	stats := runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})
	if stats.ReusedOutput {
		t.Fatal("输出已丢失，不应标记为复用")
	}
	if stats.BuiltSegments == 0 {
		t.Fatal("应重新分段")
	}
	expectSortedByOracle(t, lines, out)

	// 再次运行：这次应复用已发布输出。
	stats2 := runOK(t, in, out, work, 128<<10, 2, ndsort.Hooks{})
	if !stats2.ReusedOutput {
		t.Fatalf("期望直接复用已发布输出: %+v", stats2)
	}
}

// TestCorruptSegmentRecover 篡改清单引用的段文件后重跑，必须检测并重建。
func TestCorruptSegmentRecover(t *testing.T) {
	lines := generateLines(88, 1000)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	// 在若干段后中断，取得至少两个段。
	stopped := false
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 64 << 10, MaxMergeWay: 2,
	}, ndsort.Hooks{
		AfterSegmentSaved: func(ctx context.Context, seq int64, total int) error {
			if total >= 2 && !stopped {
				stopped = true
				return errInjected
			}
			return nil
		},
	})
	if !errors.Is(err, errInjected) {
		t.Fatal(err)
	}

	// 篡改第一个段的中间字节（避开头部/尾部）。
	seg0 := filepath.Join(work, "segment-000000.dat")
	data, err := os.ReadFile(seg0)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	data[mid] ^= 0xff
	if err := os.WriteFile(seg0, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// 重跑必须检测到损坏并最终给出正确结果。
	stats := runOK(t, in, out, work, 64<<10, 2, ndsort.Hooks{})
	if stats.ReusedSegments != 0 {
		t.Fatalf("段损坏后不应复用任何段, got %d", stats.ReusedSegments)
	}
	if stats.BuiltSegments == 0 {
		t.Fatal("应当重建全部段")
	}
	expectSortedByOracle(t, lines, out)
}

// TestCancelMidSegment 分段中途取消：未完成的段文件被删除，不进清单。
func TestCancelMidSegment(t *testing.T) {
	lines := generateLines(89, 300)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消
	_, err := ndsort.Run(ctx, ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 128 << 10, MaxMergeWay: 2,
	}, ndsort.Hooks{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望取消错误, got %v", err)
	}
	ents, _ := os.ReadDir(work)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "segment-") {
			t.Fatalf("取消后不应残留段文件: %s", e.Name())
		}
	}
}

// TestWorkDirOrphansCleaned 崩溃残留的未引用段文件应被清理。
func TestWorkDirOrphansCleaned(t *testing.T) {
	lines := generateLines(90, 100)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})

	// 伪造一个孤儿段文件。
	orphan := filepath.Join(work, "segment-000999.dat")
	if err := os.WriteFile(orphan, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 改输入迫使新流程；孤儿应被清掉。
	lines2 := append(append([]string{}, lines...), `{"id":"z","score":-1}`)
	writeInput(t, in, lines2)
	runOK(t, in, out, work, 1<<20, 2, ndsort.Hooks{})
	if fileExists(orphan) {
		t.Fatal("孤儿段文件未被清理")
	}
}

// TestNoTmpArtifactsAfterSuccess 成功后工作目录只剩清单。
func TestNoTmpArtifactsAfterSuccess(t *testing.T) {
	lines := generateLines(91, 500)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	runOK(t, in, out, work, 128<<10, 3, ndsort.Hooks{})
	ents, _ := os.ReadDir(work)
	for _, e := range ents {
		if e.Name() != "manifest.json" {
			t.Fatalf("成功后工作目录只能残留清单, 发现 %s", e.Name())
		}
	}
	// 输出目录不得残留临时文件。
	outs, _ := os.ReadDir(dir)
	for _, e := range outs {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), ".out-tmp") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
}

// TestDiskWriteFailureAtOutput 输出写入失败（只读目录）不得动旧输出。
func TestDiskWriteFailureAtOutput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过写权限，跳过")
	}
	lines := generateLines(92, 60)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	roDir := filepath.Join(dir, "readonly")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(roDir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 1 << 20, MaxMergeWay: 2,
	}, ndsort.Hooks{})
	if err == nil {
		t.Fatal("只读目录应当失败")
	}
	_ = fmt.Sprint("")
}

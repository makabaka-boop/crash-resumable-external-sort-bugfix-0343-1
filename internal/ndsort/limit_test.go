package ndsort_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ndsort/internal/ndsort"
)

// runLimit 与 runOK 相同，但额外传入每 region 上限。
func runLimit(t *testing.T, in, out, work string, mem int64, way, limit int, hooks ndsort.Hooks) *ndsort.Stats {
	t.Helper()
	stats, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath:    in,
		OutputPath:   out,
		WorkDir:      work,
		MaxSortBytes: mem,
		MaxMergeWay:  way,
		RegionLimit:  limit,
	}, hooks)
	if err != nil {
		t.Fatalf("Run(limit=%d) 失败: %v", limit, err)
	}
	return stats
}

// oracleLimit 在全量有序参考结果上执行前 N 名 + 第 N 名同分保留。
func oracleLimit(t *testing.T, lines []string, limit int) []string {
	t.Helper()
	sorted, err := oracleSort(lines)
	if err != nil {
		t.Fatalf("oracle 解析失败: %v", err)
	}
	if limit <= 0 {
		return sorted
	}
	type gk struct {
		null bool
		v    string
	}
	seen := map[gk]int{}
	cutoff := map[gk]float64{}
	out := make([]string, 0, len(sorted))
	for _, raw := range sorted {
		rec, err := ndsort.ParseRecord([]byte(raw), 1)
		if err != nil {
			t.Fatalf("oracle 二次解析失败: %v", err)
		}
		k := gk{rec.Region == nil, rec.RegionStr()}
		n := seen[k] + 1
		seen[k] = n
		keep := false
		switch {
		case n <= limit:
			keep = true
			if n == limit {
				cutoff[k] = rec.Score
			}
		case rec.Score == cutoff[k]:
			keep = true
		}
		if keep {
			out = append(out, raw)
		}
	}
	return out
}

// TestRegionLimitDifferential 是截断语义的核心对拍：
// 多种规模/内存/路数/上限下，输出必须与“全量排序后逐组截断”的参考一致。
// 极小内存会产生多个临时段，同分记录天然跨越段边界。
func TestRegionLimitDifferential(t *testing.T) {
	sizes := []int{0, 1, 5, 17, 50, 300}
	mems := []int64{1 << 20, 64 << 10}
	ways := []int{2, 3, 16}
	limits := []int{1, 2, 3, 10}
	for seed := int64(1); seed <= 2; seed++ {
		for _, n := range sizes {
			lines := generateLines(seed*7777+int64(n), n)
			for _, limit := range limits {
				want := oracleLimit(t, lines, limit)
				for _, mem := range mems {
					for _, way := range ways {
						dir := t.TempDir()
						in := filepath.Join(dir, "in.ndjson")
						out := filepath.Join(dir, "out.ndjson")
						work := filepath.Join(dir, "work")
						writeInput(t, in, lines)
						stats := runLimit(t, in, out, work, mem, way, limit, ndsort.Hooks{})
						got := readOutputLines(t, out)
						if n == 0 {
							if len(got) != 0 || stats.TotalRecords != 0 {
								t.Fatalf("seed=%d limit=%d: 空输入应产生空输出/0 统计, got=%d stats=%d",
									seed, limit, len(got), stats.TotalRecords)
							}
							continue
						}
						if len(got) != len(want) {
							t.Fatalf("seed=%d n=%d limit=%d mem=%d way=%d: 行数 %d != %d",
								seed, n, limit, mem, way, len(got), len(want))
						}
						if stats.TotalRecords != int64(len(want)) {
							t.Fatalf("seed=%d n=%d limit=%d: 统计 %d != 实际输出 %d",
								seed, n, limit, stats.TotalRecords, len(want))
						}
						for i := range want {
							if got[i] != want[i] {
								t.Fatalf("seed=%d n=%d limit=%d mem=%d way=%d 行 %d 不一致:\n got %s\nwant %s",
									seed, n, limit, mem, way, i, got[i], want[i])
							}
						}
					}
				}
			}
		}
	}
}

// TestRegionLimitTiesAcrossSegments 显式构造跨临时段的同分并列：
// 截断点之后的同分记录必须全部保留，哪怕它们位于不同的临时段。
func TestRegionLimitTiesAcrossSegments(t *testing.T) {
	big := strings.Repeat("x", 20000) // 每条约 20KiB，64KiB 内存下每段 1~2 条
	mk := func(id, region string, score int) string {
		return `{"id":"` + id + `","score":` + itoa(score) +
			`,"region":"` + region + `","payload":"` + big + `"}`
	}
	// 刻意打乱输入顺序，使同分记录落入不同段。
	lines := []string{
		mk("r3", "r", 10),
		mk("s2", "s", 5),
		mk("r1", "r", 10),
		mk("r6", "r", 9),
		mk("s1", "s", 5),
		mk("r5", "r", 10),
		mk("s3", "s", 5),
		mk("r2", "r", 10),
		mk("r4", "r", 10),
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	stats := runLimit(t, in, out, work, 64<<10, 2, 2, ndsort.Hooks{})
	if segs := stats.ReusedSegments + stats.BuiltSegments; segs < 2 {
		t.Fatalf("用例前提失败：应产生至少 2 个临时段, got %d", segs)
	}
	got := readOutputLines(t, out)
	wantIDs := map[string]bool{
		"r1": true, "r2": true, "r3": true, "r4": true, "r5": true, // 前 2 名后全部同分
		"s1": true, "s2": true, "s3": true, // s 组三条全同分
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("输出条数 %d != %d: %v", len(got), len(wantIDs), got)
	}
	if stats.TotalRecords != int64(len(wantIDs)) {
		t.Fatalf("统计 %d != 输出条数 %d", stats.TotalRecords, len(wantIDs))
	}
	for _, raw := range got {
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if !wantIDs[v.ID] {
			t.Fatalf("不应保留的记录越界: id=%s（r6 分数低应被丢弃）", v.ID)
		}
		delete(wantIDs, v.ID)
	}
	if len(wantIDs) != 0 {
		t.Fatalf("遗漏同分记录: %v", wantIDs)
	}
}

// TestRegionLimitNullAndEmptyGroups null/缺失是同一组，"" 是另一组；
// 截断后顺序仍满足全局全序："" 在前、普通串居中、null 最后。
func TestRegionLimitNullAndEmptyGroups(t *testing.T) {
	lines := []string{
		`{"id":"n1","score":1,"region":null}`,
		`{"id":"n2","score":1}`,
		`{"id":"e1","score":1,"region":""}`,
		`{"id":"e2","score":1,"region":""}`,
		`{"id":"c1","score":1,"region":"cn"}`,
		`{"id":"n3","score":0,"region":null}`,
		`{"id":"e3","score":0,"region":""}`,
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)
	stats := runLimit(t, in, out, work, 1<<20, 2, 1, ndsort.Hooks{})
	got := readOutputLines(t, out)
	wantOrder := []string{"e1", "e2", "c1", "n1", "n2"}
	if len(got) != len(wantOrder) {
		t.Fatalf("条数 %d != %d: %v", len(got), len(wantOrder), got)
	}
	if stats.TotalRecords != int64(len(wantOrder)) {
		t.Fatalf("统计 %d != %d", stats.TotalRecords, len(wantOrder))
	}
	for i, wantID := range wantOrder {
		if !strings.Contains(got[i], `"id":"`+wantID+`"`) {
			t.Fatalf("位置 %d 期望 id=%s，实际 %s", i, wantID, got[i])
		}
	}
}

// TestRegionLimitChangeInvalidatesOutput 换上限重跑不得复用旧发布文件：
// 每次结果、条数、统计与清单都必须对应当次参数。
func TestRegionLimitChangeInvalidatesOutput(t *testing.T) {
	lines := generateLines(123, 120)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	// 首次：上限 2。
	st2 := runLimit(t, in, out, work, 128<<10, 2, 2, ndsort.Hooks{})
	want2 := oracleLimit(t, lines, 2)
	if got := readOutputLines(t, out); len(got) != len(want2) {
		t.Fatalf("limit=2 条数 %d != %d", len(got), len(want2))
	}
	if st2.TotalRecords != int64(len(want2)) || st2.ReusedOutput {
		t.Fatalf("limit=2 统计异常: %+v", st2)
	}
	assertManifestLimit(t, work, 2)

	// 同参数重跑：直接复用已发布输出。
	st2b := runLimit(t, in, out, work, 128<<10, 2, 2, ndsort.Hooks{})
	if !st2b.ReusedOutput || st2b.TotalRecords != int64(len(want2)) {
		t.Fatalf("同参数重跑应复用输出: %+v", st2b)
	}

	// 改成上限 3：旧输出（段已随发布清理）必须作废、全量重建。
	st3 := runLimit(t, in, out, work, 128<<10, 2, 3, ndsort.Hooks{})
	want3 := oracleLimit(t, lines, 3)
	if st3.ReusedOutput {
		t.Fatal("换上限后绝不能复用旧输出")
	}
	if st3.BuiltSegments == 0 {
		t.Fatal("换上限后应重新分段")
	}
	assertLinesEqual(t, out, want3)
	if st3.TotalRecords != int64(len(want3)) {
		t.Fatalf("limit=3 统计 %d != %d", st3.TotalRecords, len(want3))
	}
	assertManifestLimit(t, work, 3)

	// 再改成全量（0）：同样不得复用 limit=3 的输出。
	st0 := runLimit(t, in, out, work, 128<<10, 2, 0, ndsort.Hooks{})
	want0 := oracleLimit(t, lines, 0)
	if st0.ReusedOutput {
		t.Fatal("改回全量不应复用受限输出")
	}
	assertLinesEqual(t, out, want0)
	if st0.TotalRecords != int64(len(lines)) {
		t.Fatalf("全量统计 %d != %d", st0.TotalRecords, len(lines))
	}
	assertManifestLimit(t, work, 0)

	// 再改回 2：结果必须仍与最初 limit=2 一致。
	runLimit(t, in, out, work, 128<<10, 2, 2, ndsort.Hooks{})
	assertLinesEqual(t, out, want2)
	assertManifestLimit(t, work, 2)
}

// TestRegionLimitChangeAfterCanceledSegments 分段中途取消、留下已校验临时段，
// 随后换一个上限续跑：段可复用，但输出必须按新上限截断。
func TestRegionLimitChangeAfterCanceledSegments(t *testing.T) {
	lines := generateLines(321, 400)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	// 以 limit=2 启动，建好至少 2 个段后中断；不得产生输出。
	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 64 << 10, MaxMergeWay: 2, RegionLimit: 2,
	}, ndsort.Hooks{
		AfterSegmentSaved: func(ctx context.Context, seq int64, total int) error {
			if total >= 2 {
				return errInjected
			}
			return nil
		},
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("期望注入中断, got %v", err)
	}
	if fileExists(out) {
		t.Fatal("分段阶段中断不得产生输出")
	}

	// 改选上限 3 续跑：已有段是全量有序数据，应被复用。
	st := runLimit(t, in, out, work, 64<<10, 2, 3, ndsort.Hooks{})
	if st.ReusedSegments < 2 {
		t.Fatalf("期望复用至少 2 个已校验段, got %d", st.ReusedSegments)
	}
	if st.ReusedOutput {
		t.Fatal("此前从未发布输出，不应标记复用输出")
	}
	want3 := oracleLimit(t, lines, 3)
	assertLinesEqual(t, out, want3)
	if st.TotalRecords != int64(len(want3)) {
		t.Fatalf("统计 %d != %d", st.TotalRecords, len(want3))
	}
	assertManifestLimit(t, work, 3)

	// 同参数再跑：此时应直接复用已发布输出。
	st2 := runLimit(t, in, out, work, 64<<10, 2, 3, ndsort.Hooks{})
	if !st2.ReusedOutput {
		t.Fatalf("期望复用已发布输出: %+v", st2)
	}
	assertLinesEqual(t, out, want3)
}

// TestRegionLimitChangeAfterPublishAbort 输出临时文件已写好但发布中止，
// 换上限重跑：复用段、丢弃旧临时文件，输出按新上限截断。
func TestRegionLimitChangeAfterPublishAbort(t *testing.T) {
	lines := generateLines(654, 120)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, lines)

	_, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 1 << 20, MaxMergeWay: 2, RegionLimit: 2,
	}, ndsort.Hooks{
		AfterOutputWritten: func(ctx context.Context, tmpPath string, records int64) error {
			return errInjected
		},
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("期望发布边界中断, got %v", err)
	}
	if fileExists(out) {
		t.Fatal("发布中止不得产生输出")
	}

	st := runLimit(t, in, out, work, 1<<20, 2, 1, ndsort.Hooks{})
	if st.ReusedSegments < 1 || st.BuiltSegments != 0 {
		t.Fatalf("应复用已建段且不新建段: reused=%d built=%d",
			st.ReusedSegments, st.BuiltSegments)
	}
	assertLinesEqual(t, out, oracleLimit(t, lines, 1))
	if st.TotalRecords != int64(len(oracleLimit(t, lines, 1))) {
		t.Fatalf("统计与输出不符: %+v", st)
	}
}

// TestRegionLimitEmptyInput 空输入 + 正上限：空文件、0 统计，重跑复用发布。
func TestRegionLimitEmptyInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeInput(t, in, nil)

	st := runLimit(t, in, out, work, 1<<20, 2, 3, ndsort.Hooks{})
	if st.TotalRecords != 0 {
		t.Fatalf("空输入统计应为 0, got %d", st.TotalRecords)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("空输入应产生空文件, got %q", data)
	}
	assertManifestLimit(t, work, 3)

	st2 := runLimit(t, in, out, work, 1<<20, 2, 3, ndsort.Hooks{})
	if !st2.ReusedOutput || st2.TotalRecords != 0 {
		t.Fatalf("空结果重跑应复用发布: %+v", st2)
	}

	// 换上限虽同为空，也必须按参数变化重新发布（清单口径必须与本次一致）。
	st3 := runLimit(t, in, out, work, 1<<20, 2, 5, ndsort.Hooks{})
	if st3.ReusedOutput {
		t.Fatal("空输入换上限也不应复用旧发布")
	}
	assertManifestLimit(t, work, 5)
}

// ---- 辅助 ----

func assertLinesEqual(t *testing.T, out string, want []string) {
	t.Helper()
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

func assertManifestLimit(t *testing.T, work string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(work, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		RegionLimit int `json:"region_limit"`
		Output      *struct {
			Records int64 `json:"records"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.RegionLimit != want {
		t.Fatalf("清单 region_limit=%d，期望 %d", m.RegionLimit, want)
	}
	if m.Output == nil {
		t.Fatal("清单缺少已发布输出记录")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

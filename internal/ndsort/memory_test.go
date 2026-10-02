package ndsort_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ndsort/internal/ndsort"
)

// TestMemoryBudget 验证以 32 MiB 为单次排序上限时：
//   - 刷段前的存活排序工作集（强制 GC 口径）远低于上限；
//   - 驻留记录负载估算不超过 3/4 阈值。
//
// 说明：32 MiB 约束的是“单次排序驻留内存”。Go 中未回收的解析临时
// 对象属于可回收垃圾，可用 GOGC 调节其堆积量；存活工作集才是排序本身
// 的内存占用，也是分段算法承诺的边界。
func TestMemoryBudget(t *testing.T) {
	const n = 200000
	// 每条记录约 600 字节原始负载 => 24MiB 刷段阈值下每段约 3.6 万条。
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	work := filepath.Join(dir, "work")
	writeRepeatedInput(t, in, n)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peakLive uint64 // 强制 GC 后：纯存活工作集
	var peakSeen uint64 // 不强制 GC：含待回收垃圾的口径
	sample := func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > base.HeapAlloc {
			if d := ms.HeapAlloc - base.HeapAlloc; d > peakSeen {
				peakSeen = d
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > base.HeapAlloc {
			if d := ms.HeapAlloc - base.HeapAlloc; d > peakLive {
				peakLive = d
			}
		}
	}

	stats, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath: in, OutputPath: out, WorkDir: work,
		MaxSortBytes: 32 << 20, MaxMergeWay: 16,
	}, ndsort.Hooks{
		BeforeSegmentFlush: func(ctx context.Context, records int) error {
			sample()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sample()
	t.Logf("段数=%d 估算段峰值=%d 存活工作集峰值=%d MiB 含垃圾峰值=%d MiB",
		stats.ReusedSegments+stats.BuiltSegments, stats.SegmentMaxBytes,
		peakLive>>20, peakSeen>>20)

	const capBytes = uint64(32 << 20)
	if peakLive > capBytes {
		t.Fatalf("单次排序存活工作集 %d 字节超过 32 MiB", peakLive)
	}
	// 留出排序与运行时余量：存活工作集应明显低于上限（约 3/4 阈值
	// 加记录结构体开销，正常 < 26 MiB）。
	if peakLive > 28<<20 {
		t.Fatalf("存活工作集 %d 字节离 32 MiB 上限过近，余量不足", peakLive)
	}
	// 刷段在“超过阈值的下一次 append 之后”触发，因此允许超出一个
	// 刷段阈值 + 单条记录上界；阈值本身固定为上限的 3/4 = 24 MiB。
	const flushThreshold = int64(3 * capBytes / 4)
	if stats.SegmentMaxBytes > flushThreshold+1<<20 {
		t.Fatalf("刷段驻留 %d 超过阈值 %d 过多", stats.SegmentMaxBytes, flushThreshold)
	}
	if stats.SegmentMaxBytes < flushThreshold/2 {
		t.Fatalf("刷段驻留 %d 异常偏小，阈值检查可能失效", stats.SegmentMaxBytes)
	}
}

func writeRepeatedInput(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < n; i++ {
		fmt.Fprintf(f, `{"id":"id-%04d","score":%d,"region":"c%d","payload":"%s"}`+"\n",
			i%1000, i%17, i%4, strings.Repeat("P", 500))
	}
}

// ndsort 命令对 NDJSON 文件执行受限内存的稳定外部排序。
//
// 用法:
//
//	ndsort -in data.ndjson -out sorted.ndjson [-mem 32MiB] [-way 16] [-work DIR]
//
// 排序键（全部满足时整体稳定）：
//  1. score 降序；
//  2. region 升序（null/缺失为空值，排在最后；"" 是普通空串，排在最前）；
//  3. id 按 UTF-8 字节序升序；
//  4. 原输入行号升序。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"ndsort/internal/ndsort"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		inPath      = flag.String("in", "", "输入 NDJSON 文件（必填）")
		outPath     = flag.String("out", "", "输出 NDJSON 文件（必填）")
		workDir     = flag.String("work", "", "工作目录（默认在输出文件旁）")
		memFlag     = flag.String("mem", "32MiB", "单次排序可用内存上限，如 32MiB/65536")
		wayFlag     = flag.Int("way", 16, "归并路数上限（2..16）")
		regionLimit = flag.Int("region-limit", 0, "每个 region 的输出条数上限（0 表示全部）")
	)
	flag.Parse()

	if *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须同时提供 -in 与 -out")
		flag.Usage()
		return 2
	}
	maxSort, err := ndsort.ParseSize(*memFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 2
	}
	// 软内存上限 = 排序工作集上限 + 运行时/IO 缓冲余量。
	// 排序驻留数据在达到约 3/4 maxSort 时即刷段，因此总堆保持在上限内。
	const runtimeHeadroom = 16 << 20
	debug.SetMemoryLimit(maxSort + runtimeHeadroom)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stats, err := ndsort.Run(ctx, ndsort.Options{
		InputPath:    *inPath,
		OutputPath:   *outPath,
		WorkDir:      *workDir,
		MaxSortBytes: maxSort,
		MaxMergeWay:  *wayFlag,
		RegionLimit:  *regionLimit,
	}, ndsort.Hooks{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "已取消；已完成的临时段已保留，再次运行可续跑")
			return 130
		}
		fmt.Fprintln(os.Stderr, "失败:", err)
		return 1
	}

	limitNote := ""
	if *regionLimit > 0 {
		limitNote = fmt.Sprintf("，每 region 前 %d 名（含同分并列）", *regionLimit)
	}
	fmt.Fprintf(os.Stdout,
		"完成: %d 条记录, %d 个临时段（复用 %d，新建 %d）, %d 次归并, 段峰值约 %d 字节%s%s\n",
		stats.TotalRecords,
		stats.ReusedSegments+stats.BuiltSegments, stats.ReusedSegments, stats.BuiltSegments,
		stats.MergeRuns, stats.SegmentMaxBytes,
		limitNote,
		map[bool]string{true: "（复用已发布输出）", false: ""}[stats.ReusedOutput],
	)
	return 0
}

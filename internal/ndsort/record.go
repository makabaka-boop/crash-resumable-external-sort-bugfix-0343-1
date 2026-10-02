// Package ndsort 实现 NDJSON 外部排序引擎：
// 受限内存分段、带校验的临时段、最多 16 路归并、原子发布。
package ndsort

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ParseError 描述输入文件某一行的解析错误。
type ParseError struct {
	Line    int64
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("第 %d 行解析失败: %s", e.Line, e.Message)
}

// recordOverhead 是一条记录除 Raw/ID/Region 负载外的保守开销估算：
// Record 结构体本身（约 48 字节）、字符串头、指针、解析临时对象等。
const recordOverhead = 96

// Record 是一条解析后的 NDJSON 记录。Raw 保留原始行字节，
// 最终输出始终使用 Raw，不做重新序列化。
type Record struct {
	ID     string
	Score  float64
	Region *string // nil 表示 JSON null 或字段缺失；空串 "" 是普通值
	LineNo int64
	Raw    string
}

type rawRecord struct {
	ID     *string  `json:"id"`
	Score  *float64 `json:"score"`
	Region *string  `json:"region"`
}

// ParseRecord 解析单行 NDJSON（不含行尾换行）。
// 要求：顶层必须是对象；id 必须是字符串；score 必须是数字。
// region 可空：缺省或 null 均视为空值（排序时排在最后）。
func ParseRecord(line []byte, lineNo int64) (*Record, error) {
	var rr rawRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&rr); err != nil {
		return nil, &ParseError{Line: lineNo, Message: err.Error()}
	}
	// 一行必须恰好包含一个 JSON 值，不允许尾随垃圾数据。
	if dec.More() {
		return nil, &ParseError{Line: lineNo, Message: "单个 JSON 值之后存在多余数据"}
	}
	if rr.ID == nil {
		return nil, &ParseError{Line: lineNo, Message: "缺少字符串字段 id"}
	}
	if rr.Score == nil {
		return nil, &ParseError{Line: lineNo, Message: "缺少数字字段 score"}
	}
	return &Record{
		ID:     *rr.ID,
		Score:  *rr.Score,
		Region: rr.Region,
		LineNo: lineNo,
		Raw:    string(line),
	}, nil
}

// CompareKeys 定义全序比较：
//  1. score 降序；
//  2. region 升序，空值（null/缺失）排在最后；
//  3. id 按 UTF-8 字节序升序；
//  4. 原输入行号升序（保证完全相同的键仍然稳定）。
//
// 返回负数表示 a 应排在 b 前面。
func CompareKeys(a, b *Record) int {
	if a.Score > b.Score {
		return -1
	}
	if a.Score < b.Score {
		return 1
	}
	an, bn := a.Region == nil, b.Region == nil
	switch {
	case an && !bn:
		return 1 // 空值最后
	case !an && bn:
		return -1
	case !an && !bn:
		if *a.Region < *b.Region {
			return -1
		}
		if *a.Region > *b.Region {
			return 1
		}
	}
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	if a.LineNo < b.LineNo {
		return -1
	}
	if a.LineNo > b.LineNo {
		return 1
	}
	return 0
}

// EstimateBytes 粗略估算记录在 Go 堆上占用的字节数，用于决定何时刷段。
// 估算偏大（可能重复计算 slice 头与底层数组），只会更保守地提前刷段。
func (r *Record) EstimateBytes() int {
	return len(r.Raw) + len(r.ID) + len(r.RegionStr()) + recordOverhead
}

// RegionStr 返回 region 值；空值返回空串（仅用于估算）。
func (r *Record) RegionStr() string {
	if r.Region == nil {
		return ""
	}
	return *r.Region
}

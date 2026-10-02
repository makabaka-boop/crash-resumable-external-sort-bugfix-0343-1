package ndsort_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"ndsort/internal/ndsort"
)

// ---- 对拍用的参考实现（小输入，全量内存稳定排序） ----

type oracleRecord struct {
	line   int64
	raw    string
	parsed *ndsort.Record
}

func oracleSort(lines []string) ([]string, error) {
	recs := make([]oracleRecord, 0, len(lines))
	for i, ln := range lines {
		rec, err := ndsort.ParseRecord([]byte(ln), int64(i+1))
		if err != nil {
			return nil, err
		}
		recs = append(recs, oracleRecord{line: int64(i + 1), raw: ln, parsed: rec})
	}
	// 对拍要求：忽略行号后完全相同的键也必须按输入顺序稳定排列。
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i].parsed, recs[j].parsed
		return compareWithoutLine(a, b) < 0
	})
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.raw
	}
	return out, nil
}

func compareWithoutLine(a, b *ndsort.Record) int {
	if a.Score > b.Score {
		return -1
	}
	if a.Score < b.Score {
		return 1
	}
	an, bn := a.Region == nil, b.Region == nil
	switch {
	case an && !bn:
		return 1
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
	return 0
}

// ---- 数据生成 ----

var idPool = []string{
	"", "a", "b", "abc", "zzz", "001", "世界", "日本", "café",
	"éclair", "α", "βγ", "\xff\xfe-invalid-utf8", "München",
	"foo", "foobar", "FOO", "Foo",
}
var regionPool = []*string{
	nil,
	strPtr(""),
	strPtr("cn"),
	strPtr("us"),
	strPtr("eu"),
	strPtr("ap"),
	strPtr("cn-north"),
	strPtr("世界"),
	strPtr("AA"),
}

func strPtr(s string) *string { return &s }

type genLine struct {
	id     string
	score  float64
	region *string
}

func genJSON(g *genLine) string {
	// 保持对象字段顺序固定，便于人工检查。
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	idJSON, _ := json.Marshal(g.id)
	sb.Write(idJSON)
	sb.WriteString(`,"score":`)
	sb.WriteString(fmt.Sprintf("%.6f", g.score))
	if g.region == nil {
		sb.WriteString(`,"region":null}`)
	} else {
		rb, _ := json.Marshal(*g.region)
		sb.WriteString(`,"region":`)
		sb.Write(rb)
		sb.WriteByte('}')
	}
	return sb.String()
}

func generateLines(seed int64, n int) []string {
	rng := rand.New(rand.NewSource(seed))
	lines := make([]string, n)
	// 刻意生成大量重复键，以检验稳定性：分数只在小集合内取。
	scores := []float64{1, 1, 2, 2, 3, 0, -1.5, 100, 1.5, 1.5}
	for i := 0; i < n; i++ {
		// 部分记录完全同键（同分数/region/id），依赖行号区分。
		var g genLine
		g.id = idPool[rng.Intn(len(idPool))]
		g.score = scores[rng.Intn(len(scores))]
		rg := regionPool[rng.Intn(len(regionPool))]
		g.region = rg
		if rg != nil && rng.Intn(3) == 0 {
			// 偶尔使用缺失 region 的写法。
			lines[i] = jsonMust(genLineNoRegion{ID: g.id, Score: g.score})
			continue
		}
		lines[i] = genJSON(&g)
	}
	return lines
}

type genLineNoRegion struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

func jsonMust(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeInput(t *testing.T, path string, lines []string) {
	t.Helper()
	data := strings.Join(lines, "\n")
	if len(lines) > 0 {
		data += "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readOutputLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if len(data) == 0 {
		return []string{}
	}
	return strings.Split(string(data), "\n")
}

func runOK(t *testing.T, in, out, work string, mem int64, way int, hooks ndsort.Hooks) *ndsort.Stats {
	t.Helper()
	stats, err := ndsort.Run(context.Background(), ndsort.Options{
		InputPath:    in,
		OutputPath:   out,
		WorkDir:      work,
		MaxSortBytes: mem,
		MaxMergeWay:  way,
	}, hooks)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	return stats
}

// TestSmallDifferential 是核心对拍：多种规模、内存上限、归并路数，
// 与全量内存稳定排序逐字节比较。
func TestSmallDifferential(t *testing.T) {
	sizes := []int{0, 1, 2, 5, 17, 50, 100, 300}
	mems := []int64{1 << 20, 256 << 10, 64 << 10}
	ways := []int{2, 3, 16}
	for seed := int64(1); seed <= 4; seed++ {
		for _, n := range sizes {
			lines := generateLines(seed*1000+int64(n), n)
			want, err := oracleSort(lines)
			if err != nil {
				t.Fatalf("oracle 解析失败: %v", err)
			}
			for _, mem := range mems {
				for _, way := range ways {
					dir := t.TempDir()
					in := filepath.Join(dir, "in.ndjson")
					out := filepath.Join(dir, "out.ndjson")
					work := filepath.Join(dir, "work")
					writeInput(t, in, lines)
					runOK(t, in, out, work, mem, way, ndsort.Hooks{})
					got := readOutputLines(t, out)
					if n == 0 {
						if len(got) != 0 {
							t.Fatalf("seed=%d n=%d: 空输入应产生空输出, got=%q", seed, n, got)
						}
						continue
					}
					if len(got) != len(want) {
						t.Fatalf("seed=%d n=%d mem=%d way=%d: 行数 %d != %d",
							seed, n, mem, way, len(got), len(want))
					}
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("seed=%d n=%d mem=%d way=%d 行 %d 不一致:\n got %s\nwant %s",
								seed, n, mem, way, i, got[i], want[i])
						}
					}
				}
			}
		}
	}
}

// TestRegionOrdering 显式验证 region 语义："" 在前、非空按字节序、null 最后。
func TestRegionOrdering(t *testing.T) {
	lines := []string{
		`{"id":"a","score":1,"region":null}`,
		`{"id":"b","score":1,"region":""}`,
		`{"id":"c","score":1,"region":"us"}`,
		`{"id":"d","score":1}`, // 缺失 == null
		`{"id":"e","score":1,"region":"cn"}`,
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	writeInput(t, in, lines)
	runOK(t, in, out, filepath.Join(dir, "w"), 1<<20, 2, ndsort.Hooks{})
	got := readOutputLines(t, out)
	order := []string{"b", "e", "c", "a", "d"}
	for i, wantID := range order {
		if !strings.Contains(got[i], fmt.Sprintf(`"id":"%s"`, wantID)) {
			t.Fatalf("位置 %d 期望 id=%s，实际 %s", i, wantID, got[i])
		}
	}
}

// TestScoreOrderAndStability 验证降序与同键稳定性。
func TestScoreOrderAndStability(t *testing.T) {
	var lines []string
	// 同 score/region/id 的四条记录，原始顺序必须保留。
	lines = append(lines,
		`{"id":"x","score":5,"region":"r","payload":1}`,
		`{"id":"x","score":5,"region":"r","payload":2}`,
		`{"id":"x","score":5,"region":"r","payload":3}`,
		`{"id":"x","score":5,"region":"r","payload":4}`,
		`{"id":"x","score":9,"region":"r","payload":5}`,
	)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	writeInput(t, in, lines)
	runOK(t, in, out, filepath.Join(dir, "w"), 1<<20, 2, ndsort.Hooks{})
	got := readOutputLines(t, out)
	wantPayload := []string{"5", "1", "2", "3", "4"}
	for i, p := range wantPayload {
		var v map[string]any
		if err := json.Unmarshal([]byte(got[i]), &v); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%v", v["payload"]) != p {
			t.Fatalf("位置 %d 期望 payload=%s，实际 %s", i, p, got[i])
		}
	}
}

// TestOutputIsSortedBytes 输出必须是原始行（原样字节）。
func TestOutputBytesPreserved(t *testing.T) {
	lines := []string{
		`{"id":"z","score":1}`,
		`{ "id" : "a" , "score" : 2 , "extra" : [1,2] }`,
		`{"id":"a","score":1}`,
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ndjson")
	out := filepath.Join(dir, "out.ndjson")
	writeInput(t, in, lines)
	runOK(t, in, out, filepath.Join(dir, "w"), 1<<20, 2, ndsort.Hooks{})
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{lines[1], lines[2], lines[0]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("行 %d 原始字节被改动:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// TestParseErrors 覆盖非法输入。
func TestParseErrors(t *testing.T) {
	bad := []string{
		``,
		`not json`,
		`[1,2,3]`,
		`null`,
		`{"id":"a"}`,
		`{"id":"a","score":"x"}`,
		`{"id":1,"score":1}`,
		`{"id":"a","score":1} garbage`,
		`{"score":1}`,
	}
	for i, bl := range bad {
		dir := t.TempDir()
		in := filepath.Join(dir, "in.ndjson")
		out := filepath.Join(dir, "out.ndjson")
		writeInput(t, in, []string{`{"id":"ok","score":1}`, bl})
		_, err := ndsort.Run(context.Background(), ndsort.Options{
			InputPath: in, OutputPath: out,
			WorkDir: filepath.Join(dir, "w"), MaxSortBytes: 1 << 20, MaxMergeWay: 2,
		}, ndsort.Hooks{})
		if err == nil {
			t.Fatalf("bad[%d]=%q 应当报错", i, bl)
		}
	}
}

// ensure bytes import used
var _ = bytes.Compare

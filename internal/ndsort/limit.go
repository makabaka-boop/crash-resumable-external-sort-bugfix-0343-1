package ndsort

// regionGate 在“已经按全局全序排好”的记录流上执行每个 region 的前 N 名截断，
// 第 N 名同分的记录一并保留。
//
// 关键约束：截断只能作用于全局有序流（最终归并产物）。单个临时段内看不到
// 其他段中的同分记录，若逐段截断，跨段并列会被错误丢弃，因此分段与归并
// 阶段始终保留全量数据，闸门只在发布输出时使用。
//
// region 分组语义：null 与字段缺失视为同一组（null 组）；空字符串 "" 是
// 与 null 不同的另一组；其余字面值各自成组。
type regionGate struct {
	limit  int // <=0 表示不限制，全部保留
	seen   map[groupKey]int
	cutoff map[groupKey]float64
}

// groupKey 标识一个 region 分组。null=true 时 value 无意义。
type groupKey struct {
	null  bool
	value string
}

func newRegionGate(limit int) *regionGate {
	g := &regionGate{limit: limit}
	if limit > 0 {
		g.seen = make(map[groupKey]int)
		g.cutoff = make(map[groupKey]float64)
	}
	return g
}

// keep 必须按全局全序逐条调用：记录按 score 降序到达，同组内同分记录相邻。
// 第 N 条到达时记下其 score，此后该组中所有同分记录全部放行。
func (g *regionGate) keep(r *Record) bool {
	if g.limit <= 0 {
		return true
	}
	k := groupKey{null: r.Region == nil, value: r.RegionStr()}
	n := g.seen[k] + 1
	g.seen[k] = n
	if n <= g.limit {
		if n == g.limit {
			g.cutoff[k] = r.Score
		}
		return true
	}
	return r.Score == g.cutoff[k]
}

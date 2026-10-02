package ndsort

import (
	"bufio"
	"io"
)

// lineReader 读取长度不受 bufio.Scanner 64KiB 上限约束的文本行。
// 行号从 1 开始；返回的 line 去除行尾 '\n'（以及可选的 '\r'）。
type lineReader struct {
	r      *bufio.Reader
	lineNo int64
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 128*1024)}
}

// next 返回下一行（不含行尾）。文件以空行结束时 io.EOF 与空行不会混淆：
// 空字节文件直接 EOF；"\n" 会产生一行空内容（随后 EOF）。
func (lr *lineReader) next() ([]byte, int64, error) {
	raw, err := lr.r.ReadBytes('\n')
	got := len(raw) > 0
	if got {
		lr.lineNo++
		if raw[len(raw)-1] == '\n' {
			raw = raw[:len(raw)-1]
			if len(raw) > 0 && raw[len(raw)-1] == '\r' {
				raw = raw[:len(raw)-1]
			}
		}
	}
	if err != nil {
		// ReadBytes 在返回数据的同时返回 io.EOF（最后一行无换行），
		// 先把数据交给调用方，下一次调用才返回 io.EOF。
		// ReadBytes 会自行累积超长行，不受内部缓冲区 64KiB 限制。
		if got && err == io.EOF {
			return raw, lr.lineNo, nil
		}
		return nil, lr.lineNo, err
	}
	return raw, lr.lineNo, nil
}

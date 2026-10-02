package ndsort

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
)

// 临时段/中间归并段使用统一的自描述二进制格式：
//
//	头部   : magic(8) = segMagic，version(4 BE)，reserved(4 BE)
//	记录帧 : lineNo(8 BE) + rawLen(4 BE) + rawLen 字节的原始 JSON 行
//	尾部   : magic(8) = endMagic，count(8 BE)，sha256(32)
//
// sha256 覆盖头部与全部记录帧（不含尾部本身）。
var (
	segMagic  = []byte("NDSORT1\n")
	endMagic  = []byte("NDSEND1\n")
	segHdrLen = 16
	endLen    = 8 + 8 + 32
)

const segVersion uint32 = 1

// ChecksumError 表示段文件损坏、截断或校验不匹配——出现时必须重建该段。
type ChecksumError struct {
	Path string
	Err  error
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("段文件校验失败 %s: %s", e.Path, e.Err)
}

func (e *ChecksumError) Unwrap() error { return e.Err }

type segWriter struct {
	f      *os.File
	w      *bufio.Writer
	h      hash.Hash
	path   string
	count  int64
	closed bool
}

// createSegment 以独占方式创建段文件（崩溃残留同名文件会直接报错，
// 由上层在重建前清理）。
func createSegment(path string) (*segWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	w := bufio.NewWriter(f)
	head := make([]byte, segHdrLen)
	copy(head, segMagic)
	binary.BigEndian.PutUint32(head[8:], segVersion)
	// reserved 保持 0
	if _, err := w.Write(head); err != nil {
		f.Close()
		return nil, err
	}
	h.Write(head)
	return &segWriter{f: f, w: w, h: h, path: path}, nil
}

// append 写入一帧。
func (s *segWriter) append(rec *Record) error {
	if len(rec.Raw) > 0xffffffff {
		return fmt.Errorf("记录超出帧长度上限: %d 字节", len(rec.Raw))
	}
	buf := make([]byte, 12)
	binary.BigEndian.PutUint64(buf[0:8], uint64(rec.LineNo))
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(rec.Raw)))
	if _, err := s.w.Write(buf); err != nil {
		return err
	}
	if _, err := s.w.WriteString(rec.Raw); err != nil {
		return err
	}
	s.h.Write(buf)
	s.h.Write([]byte(rec.Raw))
	s.count++
	return nil
}

// close 写尾部、刷盘并 fsync。
func (s *segWriter) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	footer := make([]byte, endLen)
	copy(footer, endMagic)
	binary.BigEndian.PutUint64(footer[8:16], uint64(s.count))
	copy(footer[16:48], s.h.Sum(nil))
	if _, err := s.w.Write(footer); err != nil {
		s.f.Close()
		return err
	}
	if err := s.w.Flush(); err != nil {
		s.f.Close()
		return err
	}
	if err := s.f.Sync(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}

// abort 放弃段并删除文件（写入失败或被取消时使用）。
func (s *segWriter) abort() {
	if s.closed {
		return
	}
	s.f.Close()
	_ = os.Remove(s.path)
}

// Segment 是校验通过后打开的只读段。
type Segment struct {
	f     *os.File
	r     *bufio.Reader
	path  string
	count int64
}

// openSegment 打开段文件并完整校验：头、全部帧、尾部计数与 sha256。
// 任何不一致都返回 *ChecksumError。
func openSegment(path string) (_ *Segment, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()

	h := sha256.New()
	head := make([]byte, segHdrLen)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, &ChecksumError{Path: path, Err: fmt.Errorf("读取头部: %w", err)}
	}
	if string(head[:8]) != string(segMagic) {
		return nil, &ChecksumError{Path: path, Err: errors.New("magic 不匹配")}
	}
	if binary.BigEndian.Uint32(head[8:12]) != segVersion {
		return nil, &ChecksumError{Path: path, Err: errors.New("版本不支持")}
	}
	h.Write(head)

	frameHdr := make([]byte, 12)
	var count int64
	for {
		if _, err := io.ReadFull(f, frameHdr); err != nil {
			return nil, &ChecksumError{Path: path, Err: fmt.Errorf("读取帧头/尾部: %w", err)}
		}
		// 合法行号不超过百万，高 4 字节为 0，不可能与 ASCII magic 冲突。
		if string(frameHdr[:8]) == string(endMagic) {
			rest := make([]byte, endLen-12)
			if _, err := io.ReadFull(f, rest); err != nil {
				return nil, &ChecksumError{Path: path, Err: fmt.Errorf("读取尾部: %w", err)}
			}
			footer := append(append([]byte{}, frameHdr...), rest...)
			footerCount := int64(binary.BigEndian.Uint64(footer[8:16]))
			if footerCount != count {
				return nil, &ChecksumError{Path: path, Err: fmt.Errorf("记录数不匹配: 帧=%d 尾部=%d", count, footerCount)}
			}
			if string(h.Sum(nil)) != string(footer[16:48]) {
				return nil, &ChecksumError{Path: path, Err: errors.New("sha256 不匹配")}
			}
			// 尾部之后不允许有多余字节。
			var one [1]byte
			if n, err := f.Read(one[:]); err != io.EOF || n != 0 {
				if err != nil {
					return nil, &ChecksumError{Path: path, Err: err}
				}
				return nil, &ChecksumError{Path: path, Err: errors.New("尾部之后存在多余字节")}
			}
			break
		}
		lineNo := int64(binary.BigEndian.Uint64(frameHdr[0:8]))
		rawLen := binary.BigEndian.Uint32(frameHdr[8:12])
		if rawLen == 0 {
			return nil, &ChecksumError{Path: path, Err: errors.New("空记录帧")}
		}
		raw := make([]byte, rawLen)
		if _, err := io.ReadFull(f, raw); err != nil {
			return nil, &ChecksumError{Path: path, Err: fmt.Errorf("记录负载截断: %w", err)}
		}
		h.Write(frameHdr)
		h.Write(raw)
		count++
		_ = lineNo
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	if _, err := io.ReadFull(br, make([]byte, segHdrLen)); err != nil {
		return nil, &ChecksumError{Path: path, Err: err}
	}
	return &Segment{f: f, r: br, path: path, count: count}, nil
}

// Path 返回段文件路径。
func (s *Segment) Path() string { return s.path }

// Count 返回校验得到的记录数。
func (s *Segment) Count() int64 { return s.count }

// Next 流式读取下一条记录，返回 io.EOF 表示到达尾部。
// openSegment 已做过完整校验，此处若解析失败意味着校验后被篡改。
func (s *Segment) Next() (*Record, error) {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(s.r, hdr); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}
	if string(hdr[:8]) == string(endMagic) {
		return nil, io.EOF
	}
	lineNo := int64(binary.BigEndian.Uint64(hdr[0:8]))
	rawLen := binary.BigEndian.Uint32(hdr[8:12])
	raw := make([]byte, rawLen)
	if _, err := io.ReadFull(s.r, raw); err != nil {
		return nil, &ChecksumError{Path: s.path, Err: fmt.Errorf("读取记录负载: %w", err)}
	}
	rec, err := ParseRecord(raw, lineNo)
	if err != nil {
		return nil, &ChecksumError{Path: s.path, Err: err}
	}
	return rec, nil
}

// Close 关闭段文件。
func (s *Segment) Close() error { return s.f.Close() }

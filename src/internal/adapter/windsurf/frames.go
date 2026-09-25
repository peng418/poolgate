package windsurf

// frames.go Connect-RPC 信封编解码（请求单信封 + 响应多帧流）。
//
// 帧格式（与参考实现 connect.js 逐字节一致）：
//
//	[1 字节 flags][4 字节大端长度][N 字节载荷]
//
// flags 位含义：
//
//	0x01 = 载荷是 gzip
//	0x02 = 流结束（trailer 帧，载荷是 JSON：成功为 {}，失败为 {"error":{...}}）
//
// 注意两点（都是「写错就静默失败」的地方）：
//  1. **请求信封不压缩**。Live 实测：gzip 过的请求帧会被上游用一个语焉不详的
//     `internal` 拒绝（参考实现 devin-connect.js 的注释写得很明确）。响应方向上游仍会
//     gzip，所以解析侧必须支持解压。
//  2. TCP 不按帧边界送达，半包（长度头到了、载荷没到齐）与粘包（一次读到好几帧）
//     都是常态。所以解析必须是「缓冲 + 尽量切」的状态机，不能按行读、也不能假定
//     一次 Read 恰好对应一帧。

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
)

// maxFrameSize 是单帧的上限（长度头声明值与解压后大小共用这一道闸）。
//
// 存在的理由与参考实现一致：防「zip 炸弹」—— 一个高压缩比的 gzip 帧在长度检查里
// 完全合法，解压却可能膨胀到 GB 级把进程 OOM 掉。解压时也要卡这个上限，不能只卡长度头。
const maxFrameSize = 16 << 20

// connectFrame 是切出来的一个完整帧。
type connectFrame struct {
	flags   byte
	payload []byte // 已解压（若 flag 带 gzip）
	trailer bool   // flags&0x02：结束元数据帧，不是内容
}

// wrapRequest 把 protobuf 字节包成**单个未压缩**的请求信封（flag=0）。
func wrapRequest(proto []byte) []byte {
	out := make([]byte, 5+len(proto))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(proto)))
	copy(out[5:], proto)
	return out
}

// frameSplitter 是流式响应的帧切分状态机。
//
// 用法：push 收到多少字节就塞多少，然后 drain 拿「当前能切出的全部完整帧」；
// 剩下的半帧留在内部缓冲区等下一批。
type frameSplitter struct {
	buf []byte
}

func (s *frameSplitter) push(b []byte) {
	s.buf = append(s.buf, b...)
}

// drain 切出全部完整帧，并把尾部半帧留在缓冲里。
//
// 返回错误只发生在「帧头声明了解压但解压失败」——那是上游/中间人的问题，
// 调用方应当终止这条流（继续读只会拿到缺数据的假答案）。
func (s *frameSplitter) drain() ([]connectFrame, error) {
	var out []connectFrame
	for len(s.buf) >= 5 {
		flags := s.buf[0]
		n := int(binary.BigEndian.Uint32(s.buf[1:5]))
		if n < 0 || n > maxFrameSize {
			return out, fmt.Errorf("windsurf: 帧长度 %d 超出上限 %d", n, maxFrameSize)
		}
		if len(s.buf) < 5+n {
			break // 载荷还没到齐：半包，留着
		}
		payload := s.buf[5 : 5+n]
		// 先解压再消费：payload 是 s.buf 的子切片，解压要读它的内容。
		if flags&0x01 != 0 {
			raw, err := gunzipBounded(payload)
			if err != nil {
				return out, fmt.Errorf("windsurf: 帧解压失败: %w", err)
			}
			payload = raw
		}
		out = append(out, connectFrame{flags: flags, payload: payload, trailer: flags&0x02 != 0})
		s.buf = s.buf[5+n:]
	}
	// 压掉已消费的前缀：切走的帧已各自持有自己的 payload（解压后是新分配；
	// 未解压时是子切片 —— 所以这里必须**拷贝**而不是原地 copy，否则会把
	// 还活着的子切片内容覆盖掉。这是本文件最容易踩的坑）。
	if len(out) > 0 && len(s.buf) > 0 {
		rest := make([]byte, len(s.buf))
		copy(rest, s.buf)
		s.buf = rest
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
	return out, nil
}

// gunzipBounded 解压一个 gzip 载荷，同时卡住**解压后**的大小上限。
func gunzipBounded(p []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	// 多读 1 字节：正好读满上限说明还有更多，即超限。
	if _, err := io.Copy(&buf, io.LimitReader(zr, maxFrameSize+1)); err != nil {
		return nil, err
	}
	if buf.Len() > maxFrameSize {
		return nil, fmt.Errorf("解压后超过 %d 字节", maxFrameSize)
	}
	return buf.Bytes(), nil
}

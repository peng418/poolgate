package windsurf

// proto.go 手搓 protobuf wire format 编解码（零依赖）。
//
// 为什么自己写而不是引 protobuf 库：
//   - 我们只用到四种 wire type（varint / fixed64 / length-delimited / fixed32），
//     整条消息的**字段号是逆向出来的常量**（写在 constants.go / protocol.go 里），
//     没有 .proto 文件，也不该凭空生成一份「看起来官方」的 schema；
//   - 参考实现（WindsurfAPI/src/proto.js）就是这么手搓的，行为可逐字节对照；
//   - 引第三方依赖会把一个「协议随时可能改」的探针渠道焊死在某个库版本上。
//
// wire format 速记（proto3）：
//
//	tag  = (field_number << 3) | wire_type   —— 用 varint 编码
//	0 = varint（int/bool/enum）  1 = fixed64（double）  2 = 长度前缀（string/bytes/子消息）
//	5 = fixed32（float）        3/4 = 已废弃的 group（我们直接报错）
//
// 本文件的输出/解析顺序都按字段号**升序**，与参考实现的「字段顺序与实测抓包一致」对齐。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// errTruncated 表示缓冲区在字段中途结束。调用方（流式解码）把它当成
// 「这一帧坏了，跳过」，绝不让它冒泡成 panic —— 上游塞一帧脏数据不该拖垮整个网关。
var errTruncated = errors.New("windsurf: protobuf 字段被截断")

// ---------------------------------------------------------------------------
// 编码
// ---------------------------------------------------------------------------

// appendVarint 追加一个 base-128 varint。
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// appendKey 追加字段头（field_number + wire_type）。
func appendKey(dst []byte, field, wire int) []byte {
	return appendVarint(dst, uint64(field)<<3|uint64(wire))
}

// appendVarintField 写一个 wire type 0 字段。
func appendVarintField(dst []byte, field int, v uint64) []byte {
	dst = appendKey(dst, field, wireVarint)
	return appendVarint(dst, v)
}

// appendBytesField 写一个 wire type 2 字段（string / bytes / 子消息都用它）。
//
// 注意：**空字符串也会写一个长度为 0 的字段**，与参考实现的 writeStringField 一致
// （只有 null/undefined 才整体跳过）。是否要写空字段由调用方决定 —— 见 appendStringIf。
func appendBytesField(dst []byte, field int, b []byte) []byte {
	dst = appendKey(dst, field, wireBytes)
	dst = appendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// appendStringField 写一个字符串字段。
func appendStringField(dst []byte, field int, s string) []byte {
	return appendBytesField(dst, field, []byte(s))
}

// appendFixed64Field 写一个 double（wire type 1，小端 IEEE-754）。
func appendFixed64Field(dst []byte, field int, v float64) []byte {
	dst = appendKey(dst, field, wireFixed64)
	return binary.LittleEndian.AppendUint64(dst, math.Float64bits(v))
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// pfield 是解析出来的一个字段。
//
//	num  : wire 0 的 varint 值
//	bytes: wire 1/2/5 的载荷（子切片，**不拷贝** —— 调用方别长期持有）
type pfield struct {
	field int
	wire  int
	num   uint64
	bytes []byte
}

// parseFields 把一段 protobuf 字节切成字段列表。
//
// 遇到 group（wire 3/4）、未知 wire type 或任何截断都返回 errTruncated —— 调用方
// （decodeFrame）据此跳过这一帧。宁可少解一帧，也不 panic。
func parseFields(buf []byte) ([]pfield, error) {
	var out []pfield
	pos := 0
	for pos < len(buf) {
		tag, n, err := readVarint(buf, pos)
		if err != nil {
			return nil, err
		}
		pos += n
		field := int(tag >> 3)
		wire := int(tag & 0x07)
		var f pfield
		f.field, f.wire = field, wire
		switch wire {
		case wireVarint:
			v, n, err := readVarint(buf, pos)
			if err != nil {
				return nil, err
			}
			pos += n
			f.num = v
		case wireFixed64:
			if pos+8 > len(buf) {
				return nil, errTruncated
			}
			f.bytes = buf[pos : pos+8]
			pos += 8
		case wireBytes:
			l, n, err := readVarint(buf, pos)
			if err != nil {
				return nil, err
			}
			pos += n
			// 长度可以任意大，但此刻必须完整落在缓冲区内（防止用超大长度跳过边界检查）。
			if l > uint64(len(buf)-pos) {
				return nil, errTruncated
			}
			f.bytes = buf[pos : pos+int(l)]
			pos += int(l)
		case wireFixed32:
			if pos+4 > len(buf) {
				return nil, errTruncated
			}
			f.bytes = buf[pos : pos+4]
			pos += 4
		default:
			return nil, fmt.Errorf("windsurf: 不支持的 wire type %d（字段 %d）", wire, field)
		}
		out = append(out, f)
	}
	return out, nil
}

// readVarint 从 off 起读一个 varint，返回（值，消耗字节数，错误）。
// 最多 10 字节（64 位），超出即视为脏数据。
func readVarint(buf []byte, off int) (uint64, int, error) {
	var v uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if off+i >= len(buf) {
			return 0, 0, errTruncated
		}
		b := buf[off+i]
		if shift == 63 && b > 1 {
			return 0, 0, fmt.Errorf("windsurf: varint 溢出")
		}
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("windsurf: varint 超过 10 字节")
}

// getBytes 取第一个匹配的 wire 2 字段载荷；没有则返回 nil。
//
// 「第一个」而不是「最后一个」：我们读的都是非 repeated 字段，上游不会重复发；
// 真重复了也只取先到的那个，不会把后到的当覆盖（protobuf 语义上是后者覆盖，
// 但这条路上没有可重复的候选字段，二者等价）。
func getBytes(fields []pfield, n int) []byte {
	b, _ := getBytesOK(fields, n)
	return b
}

// getBytesOK 同 getBytes，但额外报告「字段到底在不在」。
//
// 两者对 wire 2 不同：字段不存在与「存在但长度为 0」取到的都是空串，而工具调用里
// 空字符串（比如 arguments_json 缺席）与「显式空值」语义不同 —— 需要区分时用这个。
func getBytesOK(fields []pfield, n int) ([]byte, bool) {
	for _, f := range fields {
		if f.field == n && f.wire == wireBytes {
			return f.bytes, true
		}
	}
	return nil, false
}

// getString 取第一个匹配的 wire 2 字段并按 UTF-8 解释。
func getString(fields []pfield, n int) string {
	return string(getBytes(fields, n))
}

// getVarint 取第一个匹配的 wire 0 字段；没有则返回 (0,false)。
func getVarint(fields []pfield, n int) (uint64, bool) {
	for _, f := range fields {
		if f.field == n && f.wire == wireVarint {
			return f.num, true
		}
	}
	return 0, false
}

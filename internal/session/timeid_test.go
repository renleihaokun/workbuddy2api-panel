// timeid_test.go NewTimeOrderedID / NewSpanID 的形态契约。
//
// 这两个 ID 是「出站头族像不像官方客户端」的一部分，形态本身就是可比对的
// 特征（真机 X-Conversation-ID 恒为带连字符 UUIDv7，B3 SpanId 恒为 16 hex
// 且与 trace/request 无关）。形态回归不会被业务测试发现，必须有专门用例钉住。
package session

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// uuidV7Re 带连字符 UUIDv7 的完整形态：版本位恒 7，变体位恒 8/9/a/b。
var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// hex16Re B3 SpanId 形态：16 位小写 hex。
var hex16Re = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestNewTimeOrderedIDShape 形态必须是 36 字符的 UUIDv7（版本位 7、变体位合法）。
func TestNewTimeOrderedIDShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewTimeOrderedID()
		if len(id) != 36 {
			t.Fatalf("len=%d want 36: %q", len(id), id)
		}
		if !uuidV7Re.MatchString(id) {
			t.Fatalf("不是合法 UUIDv7: %q", id)
		}
	}
}

// TestNewTimeOrderedIDTimePrefix UUIDv7 前 48 位是毫秒时间戳——时间有序性
// 是它与「32 位随机 hex」的本质区别，也是真机形态的依据。
func TestNewTimeOrderedIDTimePrefix(t *testing.T) {
	before := time.Now().UnixMilli()
	id := NewTimeOrderedID()
	after := time.Now().UnixMilli()

	hexPrefix := strings.ReplaceAll(id, "-", "")[:12]
	var got int64
	for _, ch := range hexPrefix {
		got <<= 4
		switch {
		case ch >= '0' && ch <= '9':
			got |= int64(ch - '0')
		case ch >= 'a' && ch <= 'f':
			got |= int64(ch-'a') + 10
		default:
			t.Fatalf("非 hex 字符 %q", ch)
		}
	}
	// 允许 1ms 的边界容差：时间戳可能落在两次取样之间。
	if got < before-1 || got > after+1 {
		t.Errorf("时间戳前缀=%d 不在 [%d,%d] 之间", got, before, after)
	}
}

// TestNewTimeOrderedIDUnique 同一毫秒内也要互不相同（随机段起作用）。
func TestNewTimeOrderedIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		id := NewTimeOrderedID()
		if _, dup := seen[id]; dup {
			t.Fatalf("重复 ID: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewSpanIDShape SpanId 恒 16 位 hex。
func TestNewSpanIDShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewSpanID()
		if !hex16Re.MatchString(id) {
			t.Fatalf("不是 16 hex: %q", id)
		}
	}
}

// TestNewSpanIDNotPrefixOfMessageID B3 SpanId 不得是 messageID 的前缀切片。
//
// 真机 span 是独立随机值；用 messageID[:16] 会把 12 hex 的时间前缀带出来，
// 使两个头出现可推导关联——那是网关自造形态。这里用统计口径断言：
// 一批 SpanId 与同批 messageID 的前 16 位不应存在系统性相等。
func TestNewSpanIDNotPrefixOfMessageID(t *testing.T) {
	hits := 0
	const n = 500
	for i := 0; i < n; i++ {
		msg := NewMessageID()
		if NewSpanID() == msg[:16] {
			hits++
		}
	}
	// 16 hex 随机碰撞概率 1/2^64，500 次里出现一次都属异常。
	if hits > 0 {
		t.Errorf("SpanId 与 messageID[:16] 碰撞 %d 次，疑似仍在复用 messageID 前缀", hits)
	}
}

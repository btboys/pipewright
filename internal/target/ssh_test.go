package target

import (
	"strings"
	"testing"
)

// TestLimitedBuffer 覆盖缓冲式执行的单流上限:超限只截内容、不报错、不中断会话。
func TestLimitedBuffer(t *testing.T) {
	cases := []struct {
		name          string
		limit         int
		writes        []string
		want          string
		wantTruncated bool
	}{
		{name: "未超限:原样保留", limit: 8, writes: []string{"abc", "def"}, want: "abcdef"},
		{name: "恰好到上限:不标记截断", limit: 6, writes: []string{"abc", "def"}, want: "abcdef"},
		{name: "一次写超限:保留前缀并标记", limit: 4, writes: []string{"abcdef"}, want: "abcd", wantTruncated: true},
		{name: "多次写累计超限:保留前缀", limit: 5, writes: []string{"abc", "def", "ghi"}, want: "abcde", wantTruncated: true},
		{name: "limit<=0:回落默认上限(内容仍完整)", limit: 0, writes: []string{"hello"}, want: "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b limitedBuffer
			b.limit = c.limit
			for _, w := range c.writes {
				n, err := b.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) 返回错误:%v", w, err)
				}
				// 关键契约:即便内容被丢弃也必须报告「全部消费」。短写会让 x/crypto/ssh 内部的
				// io.Copy 报 io.ErrShortWrite,把「写满上限」误判成会话错误(命令被中断、退出码丢失)。
				if n != len(w) {
					t.Fatalf("Write(%q) = %d,want %d(必须报告全部消费)", w, n, len(w))
				}
			}
			if got := b.String(); got != c.want {
				t.Fatalf("内容 = %q,want %q", got, c.want)
			}
			if b.truncated != c.wantTruncated {
				t.Fatalf("truncated = %v,want %v", b.truncated, c.wantTruncated)
			}
		})
	}
}

// TestLimitedBufferDoesNotGrowAfterLimit 断言超限后继续写不再占用内存(上限之后只丢弃、不累积)。
func TestLimitedBufferDoesNotGrowAfterLimit(t *testing.T) {
	var b limitedBuffer
	b.limit = 4
	for _, w := range []string{"abcdef", "ghij", strings.Repeat("x", 4096)} {
		if _, err := b.Write([]byte(w)); err != nil {
			t.Fatalf("Write 返回错误:%v", err)
		}
	}
	if got := b.String(); got != "abcd" {
		t.Fatalf("内容 = %q,want %q(上限之后只丢弃不累积)", got, "abcd")
	}
	if !b.truncated {
		t.Fatal("应标记截断")
	}
}

// TestMaxExecOutputBytesBounded 断言默认单流上限存在且在一个合理量级(防误改成 0/无界)。
func TestMaxExecOutputBytesBounded(t *testing.T) {
	if maxExecOutputBytes <= 0 {
		t.Fatalf("maxExecOutputBytes = %d,必须为正(否则缓冲式执行无界吃内存)", maxExecOutputBytes)
	}
	if maxExecOutputBytes > 64<<20 {
		t.Fatalf("maxExecOutputBytes = %d,过大(单命令可吃的内存上限应保持在几十 MiB 内)", maxExecOutputBytes)
	}
}

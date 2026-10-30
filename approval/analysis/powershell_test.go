package analysis

// PowerShell 解析器的内部护栏：编码必须是无 BOM 的 UTF-16LE（-EncodedCommand 的约定），
// 输出缓冲必须有硬上限（解析器输出异常时不能把内存吃光）。两处都不启进程。

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncodedScriptIsUTF16LE(t *testing.T) {
	script := "Write-Output '你好'\n$PSVersionTable.PSVersion"
	raw, err := base64.StdEncoding.DecodeString(encodedScript(script))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16LE 字节数应为偶数: %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	if got := string(utf16.Decode(units)); got != script {
		t.Fatalf("解码回来应逐字一致:\n got=%q\nwant=%q", got, script)
	}
}

func TestLimitedBufferCapsOutput(t *testing.T) {
	var b limitedBuffer
	const limit = 256 << 10

	if n, err := b.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("正常写入应原样返回: n=%d err=%v", n, err)
	}
	if b.overflow || b.Len() != 5 {
		t.Fatalf("未超限不应标记溢出: overflow=%v len=%d", b.overflow, b.Len())
	}

	huge := strings.Repeat("x", limit)
	if n, err := b.Write([]byte(huge)); err != nil || n != len(huge) {
		t.Fatalf("Write 必须报告完整长度（否则 io.Copy 会误判短写）: n=%d err=%v", n, err)
	}
	if !b.overflow {
		t.Fatal("超过上限应标记 overflow")
	}
	if b.Len() != limit {
		t.Fatalf("缓冲应截到 %d 字节，实际 %d", limit, b.Len())
	}
	if _, err := b.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if b.Len() != limit {
		t.Fatalf("溢出后不应继续增长，实际 %d", b.Len())
	}
}

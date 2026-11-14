package spv2

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/gorilla/websocket"
)

// 关闭语义只有一种：对端消失（RST/EPIPE/已关闭）都归一到 ErrConnClosed；真网络故障仍上抛。
func TestConnGoneClassification(t *testing.T) {
	for _, err := range []error{
		websocket.ErrCloseSent,
		net.ErrClosed,
		fmt.Errorf("write: %w", syscall.ECONNRESET),
		fmt.Errorf("write: %w", syscall.EPIPE),
	} {
		if !connGone(err) {
			t.Fatalf("%v 应视为连接已关闭", err)
		}
	}
	if connGone(errors.New("some transient network error")) {
		t.Fatal("普通网络错误不应被当成关闭")
	}
	if connGone(nil) {
		t.Fatal("nil 不是关闭")
	}
}

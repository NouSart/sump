package protocol

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// idEntropy 是包级熵源：保证同毫秒内 id 单调递增。
//
// 它**不是**线程安全的——`ulid.Monotonic` 内部拿 `bufio.Reader` 包了熵源，而 bufio 的读
// 不能并发（`-race` 实测：两个 goroutine 同时 NewID 会在 bufio 里撞上）。
// 所以外面自己加一把锁；这把锁只护住"取一个 id"这几微秒，不护任何 IO。
var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewID 生成消息 / 链路 id（ULID：时间有序 + 随机，形如 01J...）。
func NewID() string {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), idEntropy).String()
}

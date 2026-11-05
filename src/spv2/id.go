package spv2

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewID 生成时间有序的 ULID。
func NewID() string {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), idEntropy).String()
}

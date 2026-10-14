// Package hub 是设置中心的核心：服务注册表、全局清单与广播。
package hub

import (
	"log"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// Conn 是 hub 对一条已连接服务的抽象。
type Conn interface {
	Send(protocol.Envelope) error
	Close() error
}

// Hub 维护已注册服务的名片、连接与心跳时间。
type Hub struct {
	mu       sync.Mutex
	cards    map[string]protocol.ServiceCard
	offline  map[string]protocol.ServiceCard // 判离线后保留的名片（墓碑：心跳晚到可复归）
	conns    map[string]Conn
	lastSeen map[string]time.Time
	static   map[string]bool // 静态名片（设置中心自身）：不参与离线扫描
	revision int64
	log      *log.Logger
}

// New 创建空的 Hub。
func New(logger *log.Logger) *Hub {
	return &Hub{
		cards:    make(map[string]protocol.ServiceCard),
		offline:  make(map[string]protocol.ServiceCard),
		conns:    make(map[string]Conn),
		lastSeen: make(map[string]time.Time),
		static:   make(map[string]bool),
		log:      logger,
	}
}

// Register 记录 / 更新一个服务的名片与连接。
// 名片内容无变化时只更新连接与心跳（重连去重）：不推进 revision、不广播。
// 返回最新快照与“名册是否变化”。
func (h *Hub) Register(card protocol.ServiceCard, conn Conn) (protocol.RosterPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, existed := h.cards[card.Name]
	changed := !existed || !reflect.DeepEqual(old, card)
	h.cards[card.Name] = card
	h.conns[card.Name] = conn
	h.lastSeen[card.Name] = time.Now()
	delete(h.offline, card.Name)
	if changed {
		h.revision++
	}
	return h.snapshotLocked(), changed
}

// Lookup 按服务名取名片（不在名册返回 false）。
func (h *Hub) Lookup(name string) (protocol.ServiceCard, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	card, ok := h.cards[name]
	return card, ok
}

// PutSelf 把设置中心自身作为一张名片放进名册（无连接、仅信息），
// 使各服务可以访问设置中心（如 jump ping）。静态名片不参与离线扫描。
func (h *Hub) PutSelf(card protocol.ServiceCard) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cards[card.Name] = card
	h.static[card.Name] = true
	h.revision++
}

// Disconnect 处理连接断开：仅移除连接映射，不视为离线
// （离线判定由心跳超时负责——适配“默认短连接”的连接模型）。
func (h *Hub) Disconnect(name string, conn Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[name]; ok && cur == conn {
		delete(h.conns, name)
	}
}

// NoteSeen 记录一次心跳。若该服务已被判离线（名片已摘、留有墓碑），
// 视为复归：恢复名片并推进 revision（返回 true 时调用方应广播）。
func (h *Hub) NoteSeen(name string, conn Conn) (protocol.RosterPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.cards[name]; ok {
		h.lastSeen[name] = time.Now()
		return protocol.RosterPayload{}, false
	}
	card, ok := h.offline[name]
	if !ok {
		return protocol.RosterPayload{}, false // 从未注册过：忽略
	}
	h.cards[name] = card
	h.conns[name] = conn
	h.lastSeen[name] = time.Now()
	delete(h.offline, name)
	h.revision++
	h.log.Printf("服务复归（心跳晚到）: %s", name)
	return h.snapshotLocked(), true
}

// MarkOffline 摘除一个服务：名片移入墓碑、断开并清理连接、推进 revision。
// 用于告别（bye）与心跳超时两种离线。返回最新快照与是否真的摘除了某服务。
func (h *Hub) MarkOffline(name string) (protocol.RosterPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	card, ok := h.cards[name]
	if !ok {
		return protocol.RosterPayload{}, false
	}
	h.offline[name] = card
	delete(h.cards, name)
	delete(h.lastSeen, name)
	conn := h.conns[name]
	delete(h.conns, name)
	if conn != nil {
		_ = conn.Close()
	}
	h.revision++
	return h.snapshotLocked(), true
}

// Sweep 扫描心跳超时：累计未心跳超过 2 ×（名片声明的心跳间隔；未声明用 fallback）
// 的服务判离线。返回最新快照与是否有服务被摘除。
func (h *Hub) Sweep(now time.Time, fallback time.Duration) (protocol.RosterPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for name, card := range h.cards {
		if h.static[name] {
			continue
		}
		hb := time.Duration(card.HeartbeatMS) * time.Millisecond
		if hb <= 0 {
			hb = fallback
		}
		seen := h.lastSeen[name]
		if seen.IsZero() || now.Sub(seen) <= 2*hb {
			continue
		}

		h.log.Printf("判定离线: %s（心跳超时，间隔 %v）", name, hb)
		h.offline[name] = card
		delete(h.cards, name)
		delete(h.lastSeen, name)
		if conn := h.conns[name]; conn != nil {
			_ = conn.Close()
		}
		delete(h.conns, name)
		h.revision++
		changed = true
	}
	if !changed {
		return protocol.RosterPayload{}, false
	}
	return h.snapshotLocked(), true
}

// Snapshot 返回当前清单。
func (h *Hub) Snapshot() protocol.RosterPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked()
}

// Broadcast 把快照作为 roster 事件推给所有已注册连接（锁外发送，避免 IO 持锁）。
func (h *Hub) Broadcast(roster protocol.RosterPayload) {
	h.BroadcastEvent(protocol.TypeRoster, roster)
}

// BroadcastEvent 把任意事件载荷推给所有已注册连接（锁外发送，避免 IO 持锁）。
func (h *Hub) BroadcastEvent(typ string, payload any) {
	type target struct {
		name string
		conn Conn
	}
	h.mu.Lock()
	targets := make([]target, 0, len(h.conns))
	for name, c := range h.conns {
		targets = append(targets, target{name: name, conn: c})
	}
	h.mu.Unlock()

	for _, t := range targets {
		env, err := protocol.NewEnvelope(typ, "settings-center", t.name, "", payload)
		if err != nil {
			continue
		}
		if err := t.conn.Send(env); err != nil {
			h.log.Printf("%s 推送失败 (%s): %v", typ, t.name, err)
		}
	}
}

func (h *Hub) snapshotLocked() protocol.RosterPayload {
	names := make([]string, 0, len(h.cards))
	for n := range h.cards {
		names = append(names, n)
	}
	sort.Strings(names)
	services := make([]protocol.ServiceCard, 0, len(names))
	for _, n := range names {
		services = append(services, h.cards[n])
	}
	return protocol.RosterPayload{Services: services, Revision: h.revision}
}

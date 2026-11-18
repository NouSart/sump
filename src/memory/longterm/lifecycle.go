package longterm

// 长期记忆的生命周期（遗忘曲线 + 重要性 + 访问频率）。
//
// 为什么不是一条统一的「按天衰减」：人记事情不是这么记的。
// 见过一次的重要的人，几年后仍记得；每天用的琐碎信息，隔周就模糊。
// 所以时间常数 S 由三样东西放大：
//
//	S = 30 天 * 2^(访问次数-1) * (1 + 999*salience^2)
//
// salience 是「重要性」（0..1，由 reasoner 判断或 remember 显式给）；
// 访问次数是「每次真正注入上下文」的累计（Touch 只记注入，不记扫描）。
// 保留率 R = exp(-Δt/S)，再折算成排序权重：
//
//	Weight = (0.5 + 0.5*R) * (1 + 0.1*ln(1+访问次数))
//
// 下限 0.5 保证老条目不会彻底消失——降权不是删除；核心记忆（priority>0）
// 权重恒为 1，不受时间影响。
//
// 这些常量是产品参数，不是协议：改它们不需要迁移数据，也不需要别的服务配合。

import (
	"math"
	"time"
)

const (
	// LifecycleBaseDays 是新条目（访问 1 次、salience 0）的时间常数。
	LifecycleBaseDays = 30.0
	// LifecycleGrowth 是每次访问对时间常数的放大倍数。
	LifecycleGrowth = 2.0
	// SalienceBoost 是 salience=1 时时间常数的放大倍数（1000 倍 ≈ 82 年）。
	SalienceBoost = 1000.0
	// AccessBoost 是访问次数对排序权重的对数加成系数。
	AccessBoost = 0.1
	// DecayFloor 是排序权重的下限：再旧也保留一半相关性。
	DecayFloor = 0.5
)

func clampSalience(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Stability 返回条目的时间常数 S（天）。核心记忆返回 +Inf。
func Stability(e Entry) float64 {
	if e.Priority > 0 {
		return math.Inf(1)
	}
	hits := e.AccessCount
	if hits < 1 {
		hits = 1
	}
	s := clampSalience(e.Salience)
	mult := 1 + (SalienceBoost-1)*s*s
	return LifecycleBaseDays * math.Pow(LifecycleGrowth, float64(hits-1)) * mult
}

// Retention 返回条目在 now 时刻的保留率 R ∈ [0,1]。
// 从未访问过（LastAccessAt=0）时回退到 CreatedAt；时间戳都解析失败则按「刚访问」算。
func Retention(e Entry, now time.Time) float64 {
	if e.Priority > 0 {
		return 1
	}
	last := lastSeen(e)
	if last.IsZero() {
		return 1
	}
	days := now.Sub(last).Hours() / 24
	if days <= 0 {
		return 1
	}
	return math.Exp(-days / Stability(e))
}

// Weight 返回条目用于排序的生命周期权重（>0）。核心记忆恒为 1。
func Weight(e Entry, now time.Time) float64 {
	if e.Priority > 0 {
		return 1
	}
	hits := e.AccessCount
	if hits < 0 {
		hits = 0
	}
	r := Retention(e, now)
	return (DecayFloor + (1-DecayFloor)*r) * (1 + AccessBoost*math.Log1p(float64(hits)))
}

func lastSeen(e Entry) time.Time {
	if e.LastAccessAt > 0 {
		return time.Unix(e.LastAccessAt, 0)
	}
	if e.CreatedAt == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, e.CreatedAt); err == nil {
		return ts
	}
	return time.Time{}
}

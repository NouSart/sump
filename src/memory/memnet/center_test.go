package memnet

// 各向异性修正的单元测试：放在包内，因为要直接验 centerVec / cosine。

import (
	"math"
	"testing"
)

func deg(d float64) []float32 {
	r := d * math.Pi / 180
	return []float32{float32(math.Cos(r)), float32(math.Sin(r))}
}

// TestCenteringSeparatesNarrowCone 验"减均值能把窄锥里的方向差剥出来"。
// 真实 embedding 就是各向异性的：所有向量挤在一个锥里，原始余弦都在 0.8~0.95。
func TestCenteringSeparatesNarrowCone(t *testing.T) {
	// 两条同义在锥内（+6°/+9°），一条无关在锥外（−6°）。
	a, b, other := deg(6), deg(9), deg(-6)

	rawSame := cosine(a, b)
	rawCross := cosine(a, other)
	if rawSame-rawCross >= 0.05 {
		t.Skipf("构造的锥太宽，原始间隔已够大（same=%.3f cross=%.3f）", rawSame, rawCross)
	}

	mean := []float32{(a[0] + b[0] + other[0]) / 3, (a[1] + b[1] + other[1]) / 3}
	cSame := cosine(centerVec(a, mean), centerVec(b, mean))
	cCross := cosine(centerVec(a, mean), centerVec(other, mean))

	if cSame-cCross <= 0.05 {
		t.Fatalf("中心化应把锥内 / 锥外拉开：same=%.3f cross=%.3f", cSame, cCross)
	}
	if cCross > 0.2 {
		t.Fatalf("锥外的应被推远，实际 %.3f", cCross)
	}
}

// TestCenterVecWithoutMean 语料还没有向量时（冷启动）退化成原向量归一化。
func TestCenterVecWithoutMean(t *testing.T) {
	v := []float32{3, 4}
	got := centerVec(v, nil)
	if math.Abs(float64(got[0])-0.6) > 1e-6 || math.Abs(float64(got[1])-0.8) > 1e-6 {
		t.Fatalf("无均值时应只做归一化，实际 %v", got)
	}
}

// TestTuningCenterOffSkipsMean 关掉 Center 时 meanVec 不查库（测试可控几何的开关）。
func TestTuningCenterOffSkipsMean(t *testing.T) {
	s := &Store{tune: DefaultTuning()}
	s.tune.Center = false
	if m := s.meanVec(); m != nil {
		t.Fatalf("Center=false 时不该给均值，实际 %v", m)
	}
}

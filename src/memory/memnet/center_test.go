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

// TestCenterVecWithoutMean 语料还没有向量时（冷启动）退化成原向量归一化；
// 入参保持原样（归一化只由返回值给出）。
func TestCenterVecWithoutMean(t *testing.T) {
	v := []float32{3, 4}
	got := centerVec(v, nil)
	if math.Abs(float64(got[0])-0.6) > 1e-6 || math.Abs(float64(got[1])-0.8) > 1e-6 {
		t.Fatalf("无均值时应只做归一化，实际 %v", got)
	}
	if len(v) != 2 || v[0] != 3 || v[1] != 4 {
		t.Fatalf("centerVec 改写了入参，实际 %v", v)
	}
}

// TestUnitIsPure 钉住 unit 的纯函数契约：入参一个字节都不许动，返回值是新切片。
//
// 这条不是洁癖：unit 曾经就地改写调用方的切片，而 Attach 之后那份向量还要拿去落库，
// 于是「写进库的向量」被一次比较偷偷改掉。别名关系早晚会被下一个调用方误用。
func TestUnitIsPure(t *testing.T) {
	in := []float32{3, 4, 0, -2}
	before := append([]float32(nil), in...) // 原文备份：调用后必须一字不差

	got := unit(in)

	for i := range before {
		if in[i] != before[i] {
			t.Fatalf("unit 改写了入参：第 %d 分量 %v → %v", i, before[i], in[i])
		}
	}
	if len(got) != len(in) {
		t.Fatalf("返回值长度应一致，实际 %v", got)
	}
	if &got[0] == &in[0] {
		t.Fatal("unit 与入参共用底层数组：返回值必须是新切片")
	}

	// 归一化本身仍然成立（这条测试不只是验别名）。
	var sq float64
	for _, f := range got {
		sq += float64(f) * float64(f)
	}
	if math.Abs(sq-1) > 1e-6 {
		t.Fatalf("返回值应是单位向量，模长平方 = %.6f", sq)
	}
	for i, want := range []float64{3, 4, 0, -2} {
		if diff := math.Abs(float64(got[i]) - want/math.Sqrt(29)); diff > 1e-6 {
			t.Fatalf("第 %d 分量应为 %.6f，实际 %.9f（差 %.9f）", i, want/math.Sqrt(29), got[i], diff)
		}
	}

	// 零向量：没有方向，返回值仍是零，但也不能和入参共用底层数组。
	zero := []float32{0, 0}
	gotZero := unit(zero)
	if len(gotZero) != 2 || gotZero[0] != 0 || gotZero[1] != 0 {
		t.Fatalf("零向量应原样返回，实际 %v", gotZero)
	}
	if &gotZero[0] == &zero[0] {
		t.Fatal("零向量也不该与入参共用底层数组")
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

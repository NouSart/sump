package metrics

// 与基线对比：回答"这次改动算不算改进"。
//
// 判据不是"数字变了"，而是"变化超出了噪声带"。噪声带取两次 run 的标准差里较大的那个——
// 它是同配置重复试验自然产生的波动，小于它的差异没有说服力。

import (
	"fmt"
	"strings"
)

// Delta 是当前 run 与基线的对比。
type Delta struct {
	Compatible bool   `json:"compatible"`
	Reason     string `json:"reason,omitempty"`

	PassKPrev  float64 `json:"pass_k_prev"`
	PassKNow   float64 `json:"pass_k_now"`
	PassKDelta float64 `json:"pass_k_delta"`

	HopsPrev   float64 `json:"hops_prev"`
	HopsNow    float64 `json:"hops_now"`
	HopsNoise  float64 `json:"hops_noise"`
	WallPrev   float64 `json:"wall_prev"`
	WallNow    float64 `json:"wall_now"`
	WallNoise  float64 `json:"wall_noise"`
	TokenPrev  float64 `json:"tokens_prev"`
	TokenNow   float64 `json:"tokens_now"`
	TokenNoise float64 `json:"tokens_noise"`

	// Significant 表示至少有一项超出了噪声带；否则整体算"与基线无差别"。
	Significant bool     `json:"significant"`
	Notes       []string `json:"notes,omitempty"`
}

// Compare 比较当前结果与基线；不可比时 Compatible=false 并说明原因。
//
// 可比的前提是同一批任务、同一 k、同一份数据集——换任务集或换 k 的数字本来就不能比。
func Compare(prev, cur Summary) Delta {
	d := Delta{
		PassKPrev: prev.PassK, PassKNow: cur.PassK, PassKDelta: cur.PassK - prev.PassK,
		HopsPrev: prev.MeanHops, HopsNow: cur.MeanHops,
		WallPrev: prev.MeanWallMS, WallNow: cur.MeanWallMS,
		TokenPrev: prev.MeanTokens, TokenNow: cur.MeanTokens,
	}

	switch {
	case prev.Manifest.Suite != cur.Manifest.Suite:
		d.Reason = fmt.Sprintf("任务集不同（%s vs %s）", prev.Manifest.Suite, cur.Manifest.Suite)
	case prev.Manifest.Trials != cur.Manifest.Trials:
		d.Reason = fmt.Sprintf("k 不同（%d vs %d）", prev.Manifest.Trials, cur.Manifest.Trials)
	case prev.Manifest.DatasetSHA != cur.Manifest.DatasetSHA:
		d.Reason = "数据集内容不同（sha256 不一致）"
	case len(prev.Tasks) != len(cur.Tasks):
		d.Reason = fmt.Sprintf("任务数不同（%d vs %d）", len(prev.Tasks), len(cur.Tasks))
	}
	if d.Reason != "" {
		return d
	}
	d.Compatible = true

	d.HopsNoise = maxf(prev.HopsStd, cur.HopsStd)
	d.WallNoise = maxf(prev.WallStd, cur.WallStd)
	d.TokenNoise = maxf(prev.TokenStd, cur.TokenStd)

	if d.PassKDelta != 0 {
		d.Significant = true
		d.Notes = append(d.Notes, fmt.Sprintf("pass^k %+.1f 个百分点", d.PassKDelta*100))
	}
	d.Significant = d.Significant || exceed("跳数", d.HopsNow-d.HopsPrev, d.HopsNoise, &d.Notes)
	d.Significant = d.Significant || exceed("耗时(ms)", d.WallNow-d.WallPrev, d.WallNoise, &d.Notes)
	d.Significant = d.Significant || exceed("token", d.TokenNow-d.TokenPrev, d.TokenNoise, &d.Notes)
	if !d.Significant {
		d.Notes = append(d.Notes, "各项差异都在噪声带内：这次改动没有被证明有效（也没有被证明有害）")
	}
	return d
}

// exceed 判断某项变化是否超出噪声带，并把可读结论追加到 notes。
func exceed(name string, delta, noise float64, notes *[]string) bool {
	if noise <= 0 {
		// 没有波动数据（k=1 或该指标恒定）：只能报差值，不能宣称显著。
		if delta != 0 {
			*notes = append(*notes, fmt.Sprintf("%s %+.1f（无波动数据，无法判断是否显著）", name, delta))
		}
		return false
	}
	if absf(delta) <= noise {
		return false
	}
	arrow := "变好"
	if delta > 0 {
		arrow = "变差"
	}
	*notes = append(*notes, fmt.Sprintf("%s %+.1f（噪声带 ±%.1f，%s）", name, delta, noise, arrow))
	return true
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// Markdown 把对比结果渲染成报告里的几行。
func (d Delta) Markdown() string {
	var b strings.Builder
	if !d.Compatible {
		fmt.Fprintf(&b, "### 与基线对比\n\n不可比：%s。基线数字不能拿来当本次的参照。\n", d.Reason)
		return b.String()
	}
	fmt.Fprintf(&b, "### 与基线对比\n\n")
	fmt.Fprintf(&b, "| 指标 | 基线 | 本次 | 差异 | 噪声带 |\n| --- | --- | --- | --- | --- |\n")
	fmt.Fprintf(&b, "| pass^k | %.1f%% | %.1f%% | %+.1f 个百分点 | —（比例指标，按绝对值判） |\n",
		d.PassKPrev*100, d.PassKNow*100, d.PassKDelta*100)
	fmt.Fprintf(&b, "| 平均跳数 | %.1f | %.1f | %+.1f | ±%.1f |\n", d.HopsPrev, d.HopsNow, d.HopsNow-d.HopsPrev, d.HopsNoise)
	fmt.Fprintf(&b, "| 平均耗时(ms) | %.0f | %.0f | %+.0f | ±%.0f |\n", d.WallPrev, d.WallNow, d.WallNow-d.WallPrev, d.WallNoise)
	fmt.Fprintf(&b, "| 平均 token | %.0f | %.0f | %+.0f | ±%.0f |\n", d.TokenPrev, d.TokenNow, d.TokenNow-d.TokenPrev, d.TokenNoise)
	fmt.Fprintf(&b, "\n**结论：%s**\n", conclusion(d))
	if len(d.Notes) > 0 {
		fmt.Fprintf(&b, "\n")
		for _, n := range d.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

func conclusion(d Delta) string {
	if d.Significant {
		return "有指标超出噪声带——可以认为这次改动确实改变了行为（变好还是变差看上面的正负号）"
	}
	return "未超出噪声带——不能说这次改动有效"
}

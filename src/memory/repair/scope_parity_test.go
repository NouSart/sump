package repair

// 两份手抄清单的对照。
//
// audit 包为了不共享谓词，把"扫哪张表哪一列"照本包又抄了一份（见 audit 包注释）。
// 抄的东西会漂，漂的方向是漏：本包以后补一列，audit 不知道，
// 于是"两把尺都报零"其实是"一把尺少看了一格"。这条测试把漂移变成红灯。

import (
	"sort"
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/audit"
)

func TestAuditScopeMatchesCleanedTextColumns(t *testing.T) {
	want := map[string]bool{}
	for _, tb := range tables {
		for _, c := range tb.cols {
			if c.k == kText {
				want[tb.name+"."+c.name] = true
			}
		}
	}
	got := map[string]bool{}
	for _, col := range audit.Scope() {
		key := col.Table + "." + col.Name
		if got[key] {
			t.Errorf("audit 清单里 %s 重复", key)
		}
		got[key] = true
	}

	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("两把尺的扫描范围漂了\n清洗会改写但 audit 不看：%s\naudit 在看但清洗不当正文列：%s",
			strings.Join(missing, ", "), strings.Join(extra, ", "))
	}
}

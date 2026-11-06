package redact

// redact 是共享库写入边界的规则本体，因此这组测试同时是"规则的可执行定义"：
// 改了规则，这里是第一处必须更新的地方。
//
// 样本值全部是造的形状，不对应任何真实账号 / 机器 / 密钥。

import (
	"strings"
	"testing"
)

const (
	sampleWin   = `D:\srv\app\data\x.db`
	samplePosix = `/srv/app/logs/app.log`
	sampleURL   = `ws://127.0.0.1:3000`
	sampleToken = `sk-abcdefgh12345678`
	sampleEmail = `someone@example.com`
	samplePhone = `13812345678`
)

func TestTextMasksEveryShape(t *testing.T) {
	cases := []struct{ name, in, wantContain, wantAbsence string }{
		{"Windows 路径", "打开 " + sampleWin + " 看看", "[路径]", sampleWin},
		{"UNC 路径", `看 \\fileserver\share\a.txt`, "[路径]", `\\fileserver\share`},
		{"POSIX 路径", "日志在 " + samplePosix + " 里", "[路径]", samplePosix},
		{"ws 链接", "默认地址 " + sampleURL, "[链接]", sampleURL},
		{"http 链接", "参考 https://example.com/a?b=1 的做法", "[链接]", "example.com"},
		{"密钥形状", "key 是 " + sampleToken + " 别外传", "[已隐去]", sampleToken},
		{"Bearer", "header Bearer abc.def-ghi", "[已隐去]", "abc.def-ghi"},
		{"邮箱", "发到 " + sampleEmail, "[邮箱]", sampleEmail},
		{"手机号", "打 " + samplePhone + " 找他", "[已隐去]", samplePhone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Text(c.in)
			if !strings.Contains(got, c.wantContain) {
				t.Errorf("Text(%q) = %q，期望含 %q", c.in, got, c.wantContain)
			}
			if strings.Contains(got, c.wantAbsence) {
				t.Errorf("Text(%q) = %q，原值没被洗掉: %q", c.in, got, c.wantAbsence)
			}
		})
	}
}

func TestTextKeepsOrdinaryProse(t *testing.T) {
	for _, in := range []string{
		"在改门户的时间主题",
		"shell 的退出码与 stdout 一起送达，不必再查",
		"用 PowerShell 跑命令时中文要设 UTF-8 代码页",
	} {
		if got := Text(in); got != in {
			t.Errorf("正常提炼物不该被动：Text(%q) = %q", in, got)
		}
	}
}

// Text 必须可重复作用：写入边界与服务层（算向量前先洗一次）会对同一段文本调用两次，
// 不幂等就会出现"库里一行、向量对着另一行"。
func TestTextIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"打开 " + sampleWin + " 参考 " + sampleURL + " 令牌 " + sampleToken,
		"日志在 " + samplePosix + " 联系 " + samplePhone + " 或 " + sampleEmail,
		"第一行\n第二行\r\n第三行\t缩进",
	} {
		once := Text(in)
		if twice := Text(once); twice != once {
			t.Errorf("Text 不幂等：一次 = %q，两次 = %q", once, twice)
		}
	}
}

// Detect 是"洗"的另一半：Text 负责改，Detect 负责回答"还有没有"。
// 两边必须认同一批形状，否则存量清理会出现洗完仍报干净（漏报）
// 或洗完永远不收敛（误报）——前一种正是这次事故要防的自欺。
func TestDetectAgreesWithText(t *testing.T) {
	dirty := map[string]string{
		"Windows 路径": "打开 " + sampleWin,
		"UNC 路径":     `看 \\fileserver\share\a.txt`,
		"POSIX 路径":   "日志在 " + samplePosix,
		"链接":         "默认地址 " + sampleURL,
		"密钥形状":       "key 是 " + sampleToken,
		"Bearer":     "header Bearer abc.def-ghi",
		"邮箱":         "发到 " + sampleEmail,
		"手机号":        "打 " + samplePhone + " 找他",
	}
	for name, in := range dirty {
		if Detect(in) == "" {
			t.Errorf("%s：Detect 认不出这一格是泄露（复检会假报干净）: %q", name, in)
		}
		if got := Text(in); Detect(got) != "" {
			t.Errorf("%s：Text 洗完仍被 Detect 命中（清理永不收敛）: %q → %q", name, in, got)
		}
	}
	for _, clean := range []string{
		"在改门户的时间主题", "", "   ", "shell 的退出码与 stdout 一起送达",
	} {
		if why := Detect(clean); why != "" {
			t.Errorf("正常提炼物不该被 Detect 命中（%q → %s）", clean, why)
		}
	}
}

// 标识列不看 Detect（裸账号没有形状可言），看结构：必须已经是摘要形状。
// 谓词必须与构造函数互相认——不然收口函数产出的值会被复检判成脏。
func TestOpaqueAndRefPredicatesMatchTheirConstructors(t *testing.T) {
	for _, raw := range []string{"qq:private:120000000", "web:group:42", "120000000", "  qq:x:1 "} {
		opaque, err := SessionID(raw)
		if err != nil {
			t.Fatalf("SessionID(%q): %v", raw, err)
		}
		if !IsOpaqueSessionID(opaque) {
			t.Errorf("SessionID 的输出没过自己的谓词，复检会永远不收敛: %q", opaque)
		}
		if IsOpaqueSessionID(raw) {
			t.Errorf("原始会话键不该被判成已收口: %q", raw)
		}
	}
	if !IsOpaqueSessionID("") {
		t.Error("空会话键应视为合规（memories.conversation_id 允许为空）")
	}
	for _, ok := range []string{"reasoner", "t20261019T175731-de9fa74c9a53", ""} {
		if !IsRefID(ok) {
			t.Errorf("不透明 id 被判成不合规: %q", ok)
		}
	}
	for _, bad := range []string{"a/b", "a:b", "user@example.com", "has space", `C:\x`} {
		if IsRefID(bad) {
			t.Errorf("标识列谓词接受了不该接受的值: %q", bad)
		}
	}
}

func TestTextFlattensNewlinesAndTrims(t *testing.T) {
	if got := Text("  第一行\n第二行\r\n第三行  "); got != "第一行 第二行 第三行" {
		t.Errorf("换行没压平: %q", got)
	}
	if got := Text("   \n\t "); got != "" {
		t.Errorf("空白该压成空串，实际 %q", got)
	}
}

func TestSanitizeTruncatesByRunes(t *testing.T) {
	long := strings.Repeat("经", 300)
	if got := Sanitize(long, 10); len([]rune(got)) != 10 {
		t.Errorf("按字符截断失效：len = %d", len([]rune(got)))
	}
	if got := Sanitize(long, 10); !strings.HasSuffix(got, "…") {
		t.Errorf("截断该补省略号: %q", got)
	}
	if got := Sanitize("abc", 0); got != "abc" {
		t.Errorf("max<=0 不该截断: %q", got)
	}
	// 截断按字符，不能把多字节字符切成半个（切坏会带 U+FFFD）。
	if got := Sanitize(long, 5); strings.ContainsRune(got, 0xFFFD) {
		t.Errorf("截断切坏了字符: %q", got)
	}
}

func TestSessionIDHashesAccountButKeepsEntry(t *testing.T) {
	got, err := SessionID("qq:private:987654321")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "987654321") || strings.Contains(got, "private") {
		t.Errorf("账号段与子类型都该被摘要掉: %q", got)
	}
	if !strings.HasPrefix(got, "qq:") {
		t.Errorf("入口该留着（它是服务名）: %q", got)
	}
	// EntryOf 依赖冒号前的入口段，收口后仍能解析——目录展示靠它。
	if i := strings.IndexByte(got, ':'); i <= 0 {
		t.Errorf("收口形状丢了入口段: %q", got)
	}
}

func TestSessionIDIsStableAndIdempotent(t *testing.T) {
	first, err := SessionID("qq:private:987654321")
	if err != nil {
		t.Fatal(err)
	}
	second, err := SessionID("qq:private:987654321")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("同一会话每次算出的键必须相同，否则目录按会话归并会散: %q vs %q", first, second)
	}
	// 可重复作用：目录里读回的键会被原样传回来删除；再摘要一次就删不到自己了。
	again, err := SessionID(first)
	if err != nil {
		t.Fatalf("对收口后的键再作用一次不该报错: %v", err)
	}
	if again != first {
		t.Errorf("SessionID 不幂等: %q -> %q", first, again)
	}
	// 不同账号必须落到不同键（否则两个会话会被并成一条）。
	other, err := SessionID("qq:group:123456789")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("不同会话撞到了同一个键")
	}
}

func TestSessionIDEdges(t *testing.T) {
	if got, err := SessionID(""); err != nil || got != "" {
		t.Errorf("空入参该得空值: %q, %v", got, err)
	}
	if got, err := SessionID("  "); err != nil || got != "" {
		t.Errorf("空白入参该得空值: %q, %v", got, err)
	}
	// 没有入口段的 id：仍然收口，但不该凭空造一个入口（展示会多出假分类）。
	got, err := SessionID("default")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, ":") {
		t.Errorf("无入口的 id 不该被安上入口: %q", got)
	}
	if _, err := SessionID("web:private:owner@example.com"); err == nil {
		t.Error("含邮箱的会话 id 必须拒绝")
	}
	if _, err := SessionID(sampleWin); err == nil {
		t.Error("含路径的会话 id 必须拒绝")
	}
	if _, err := SessionID("qq:"); err == nil {
		t.Error("只有入口没有账号段，无法归并，必须拒绝")
	}
	if _, err := SessionID("C:\\Users\\bob:x"); err == nil {
		t.Error("入口段不是服务名形状，必须拒绝")
	}
}

func TestRefIDAcceptsOpaqueRejectsEverythingElse(t *testing.T) {
	for _, ok := range []string{
		"t20261101T170150-de9fa74c9a53",
		"n20261101T170150-aabbccddeeff",
		"reasoner", "agent", "t1",
	} {
		if got, err := RefID(ok, "task_id"); err != nil || got != ok {
			t.Errorf("不透明 id 该原样通过: %q -> %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"qq:private:987654321", sampleWin, samplePosix, sampleEmail,
		"", // 空由下面的用例单独断言
		"a b", "x/y", "x\\y", strings.Repeat("长", 40),
	} {
		if bad == "" {
			continue
		}
		if _, err := RefID(bad, "task_id"); err == nil {
			t.Errorf("RefID 放过了 %q", bad)
		}
	}
	if got, err := RefID("", "task_id"); err != nil || got != "" {
		t.Errorf("空标识该得空值: %q, %v", got, err)
	}
}

func TestRefIDErrorNamesTheColumn(t *testing.T) {
	// 错误要指明对象（仓库规矩），否则运维只能猜是哪一列炸了。
	_, err := RefID(sampleWin, "mem_entries.task_id")
	if err == nil || !strings.Contains(err.Error(), "mem_entries.task_id") {
		t.Fatalf("错误信息没指明列: %v", err)
	}
}

func TestCapabilitiesStaysAServiceNameList(t *testing.T) {
	got := Capabilities([]string{"Reasoner", "reasoner", "files", "  ", "C:\\a", "shell", "a", "b", "c", "d", "e", "f", "g"})
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "\\") || strings.Contains(joined, "C:") {
		t.Errorf("能力清单混进了路径: %v", got)
	}
	if len(got) != 6 {
		t.Errorf("封顶 6 个失效: %v", got)
	}
	if got[0] != "reasoner" {
		t.Errorf("该小写归一并去重: %v", got)
	}
	for _, c := range got {
		if c == "reasoner" && got[1] == "reasoner" {
			t.Fatal("去重失效")
		}
	}
}

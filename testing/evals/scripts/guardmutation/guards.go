package main

// 异变清单：本轮整改立起来的每一道守卫，各配一条"看起来合理但语义错了"的改动。
//
// 为什么要单独一份而不是只留 evals 那套：evals 的清单守的是评测器自己的判定，
// 而这轮整改改的是**生产路径上的安全与正确性判断**（写入边界、SSRF 守卫、协议帧、
// 审批档位、送达判定）。这些判断的测试是本轮新写的，"测试写完了"不等于"测试守得住"——
// 只有把守卫本身改错、看测试是否翻脸，才知道红线是真有人看着还是写在注释里。
//
// 每条的选法都一样：挑那种**改完代码仍能编译、仍能跑、看上去还说得通**的写法。
// 随机改字符没意义，改出来的编译错误只会证明编译器在工作。
var guards = []guard{
	// ---------- 共享库写入边界（src/memory/redact）----------
	{
		name: "复检谓词认不出 UNC 路径，存量清理自称干净",
		file: "src/memory/redact/redact.go",
		old:  `	case reWinPath.MatchString(s), reUNCPath.MatchString(s), rePosix.MatchString(s):`,
		new:  `	case reWinPath.MatchString(s), rePosix.MatchString(s):`,
	},
	{
		name: "密钥形状不再算泄露（sk-… 原样进库）",
		file: "src/memory/redact/redact.go",
		old:  `	case reToken.MatchString(s), reBearer.MatchString(s):`,
		new:  `	case reBearer.MatchString(s):`,
	},
	{
		name: "会话键判定放宽：任何值都算已收口",
		file: "src/memory/redact/redact.go",
		old:  `	return reOpaqueSession.MatchString(s) || reOpaqueBare.MatchString(s) || s == ""`,
		new:  `	return true`,
	},
	{
		name: "摘要位数变短，产出的形状不再是自己认的那一种",
		file: "src/memory/redact/redact.go",
		old:  `	return hex.EncodeToString(sum[:])[:12]`,
		new:  `	return hex.EncodeToString(sum[:])[:8]`,
	},
	{
		name: "标识列不再收口：带冒号 / at 的值照样写进 task_id",
		file: "src/memory/redact/redact.go",
		old:  `	if !reRefID.MatchString(s) {`,
		new:  `	if false && !reRefID.MatchString(s) {`,
	},

	// ---------- 会话目录的两个标题列（src/memory/shared）----------
	// 用例先填实（d3174af：Publish 输入里两个标题原本都是空串，断言对着空格永真），再补守卫。
	{
		name: "任务标题绕过写入边界（目录里的标题原样入库）",
		file: "src/memory/shared/sessions.go",
		old:  `	title := Sanitize(in.TaskTitle, maxTitleChars)`,
		new:  `	title := in.TaskTitle`,
	},
	{
		name: "会话标题绕过写入边界（门户上那行会话名没人管）",
		file: "src/memory/shared/sessions.go",
		old:  `	sessionTitle := Sanitize(in.SessionTitle, maxTitleChars)`,
		new:  `	sessionTitle := in.SessionTitle`,
	},

	// ---------- 存量清理（src/memory/repair）----------
	{
		name: "复检没归零也照样提交（洗了一半写进库）",
		file: "src/memory/repair/repair.go",
		old:  `		if len(rep.Remaining) > 0 {`,
		new:  `		if false && len(rep.Remaining) > 0 {`,
	},
	{
		name: "跳过复检，报告里的「剩余」永远是空",
		file: "src/memory/repair/repair.go",
		old:  `	rep.Remaining, err = verifyFn(tx)`,
		new:  `	rep.Remaining, err = nil, nil`,
	},
	{
		name: "清理清单漏掉一列（那张表永远不被检）",
		file: "src/memory/repair/repair.go",
		old:  `		{"anchor", kText},`,
		new:  `		// anchor 是种子句，写死的，不用洗`,
	},

	// ---------- 经验网向量与出厂参数（src/memory/memnet）----------
	{
		name: "unit 回到就地改写（调用方手里的向量被偷偷归一化）",
		file: "src/memory/memnet/memnet.go",
		old: `	out := make([]float32, len(v))
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		copy(out, v)
		return out
	}
	n = math.Sqrt(n)
	for i := range v {
		out[i] = float32(float64(v[i]) / n)
	}
	return out`,
		new: `	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v`,
	},
	{
		name: "加挂阈值高过中心化后的带宽（「网」悄悄退化成单父树）",
		file: "src/memory/memnet/memnet.go",
		old:  `		LinkL1:      0.15,`,
		new:  `		LinkL1:      0.25,`,
	},

	// ---------- 出站下载守卫（src/protocol/fetchsafe）----------
	{
		name: "协议白名单放开：file:// 也算 http",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `	if u.Scheme != "http" && u.Scheme != "https" {`,
		new:  `	if u.Scheme == "" {`,
	},
	{
		name: "链路本地不再拒（云元数据地址 169.254.169.254 放行）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old: `	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地地址"`,
		new: `	case ip.IsLinkLocalMulticast():
		return "组播地址"`,
	},
	{
		name: "重定向跳按逃生闸口径检查（借中转页回到本机）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `			if r := p.check(req.URL.String(), false); r != nil {`,
		new:  `			if r := p.check(req.URL.String(), p.AllowLoopback); r != nil {`,
	},
	{
		name: "超限探测的余量被省掉（超长内容被静默截断成「合法」）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old:  `	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))`,
		new:  `	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))`,
	},
	{
		name: "拨号前不再复查（校验与连接之间换答案）",
		file: "src/protocol/fetchsafe/fetchsafe.go",
		old: `		if ip := net.ParseIP(host); ip != nil {
			if why := dangerOf(ip, p.AllowLoopback); why != "" {`,
		new: `		if ip := net.ParseIP(host); ip != nil {
			if why := dangerOf(ip, true); why != "" {`,
	},

	// ---------- 协议参考实现（src/spv2）----------
	{
		name: "reply_to 的 msgpack 键名漂成驼峰（对端读不到关联）",
		file: "src/spv2/frame.go",
		old:  `	ReplyTo        string  ` + "`" + `msgpack:"reply_to,omitempty" json:"reply_to,omitempty"` + "`",
		new:  `	ReplyTo        string  ` + "`" + `msgpack:"replyTo,omitempty" json:"replyTo,omitempty"` + "`",
	},
	{
		name: "未知 method 回了错误但码不是 not_found",
		file: "src/spv2/server.go",
		old:  `			_ = c.send(NewErrorReply(f, s.card.Actor, CodeNotFound, "unknown method: "+f.Method, false, nil))`,
		new:  `			_ = c.send(NewErrorReply(f, s.card.Actor, CodeInternal, "unknown method: "+f.Method, false, nil))`,
	},
	{
		name: "幂等缓存对所有 method 生效（有副作用的 method 被重放）",
		file: "src/spv2/server.go",
		old:  `		if m, ok := s.card.Method(f.Method); ok && m.Idempotent {`,
		new:  `		if _, ok := s.card.Method(f.Method); ok {`,
	},

	// ---------- 隔离与记账（src/spv2 · 由 P3 补上）----------
	{
		name: "panic 被静默吞掉：不记账也不说是怎么回事",
		file: "src/spv2/server.go",
		old:  `	s.metrics.Counter("sump_panics_total", "Recovered handler panics", map[string]string{"method": f.Method}, 1)`,
		new:  `	// 只隔离，不记账`,
	},
	{
		name: "校验通过的 reply/stream 帧又被静默丢弃",
		file: "src/spv2/server.go",
		old:  `		s.countDropped(f, dropNoDispatcher, "")`,
		new:  `		_ = f`,
	},
	{
		name: "帧丢了只进指标不进日志（聚合看得见，单条查不着是谁丢的）",
		file: "src/spv2/server.go",
		old:  `	s.logger.Printf("%s", msg)`,
		new:  `	_ = msg`,
	},
	{
		name: "panic 的回执没算回执：event 那一半被记两次",
		file: "src/spv2/server.go",
		old: `			panicked = true
			s.countPanic(call.Frame, r)`,
		new: `			s.countPanic(call.Frame, r)`,
	},
	{
		name: "校验失败的 call 不再补错误回执（账记了，调用方却还在空等）",
		file: "src/spv2/server.go",
		old:  `	if f.Type == TypeCall {`,
		new:  `	if false {`,
	},

	// ---------- 会话库两张脸（src/memory/store）----------
	{
		name: "归档的会话又冒回默认列表",
		file: "src/memory/store/conversations.go",
		old:  `	if !includeArchived {`,
		new:  `	if false && !includeArchived {`,
	},

	// ---------- 复盘归类（src/reasoner/server）----------
	{
		name: "偏好被降回遗忘曲线（「不该忘」变成会忘）",
		file: "src/reasoner/server/reflect.go",
		old:  `			dur = "core"`,
		new:  `			dur = "durable"`,
	},

	// ---------- 审批档位（src/approval/engine）----------
	{
		name: "中档黑名单漏掉一条系统级动作（档位再次倒挂）",
		file: "src/approval/engine/tier_medium.go",
		old:  `	"pkexec": "提权执行",`,
		new:  `	// pkexec 少见，先不列`,
	},

	// ---------- 送达判定（src/qq/napcat）----------
	{
		name: "没有 status 也没有 retcode 时当成送达",
		file: "src/qq/napcat/napcat.go",
		old:  `	if !hasStatus && !hasRetcode {`,
		new:  `	if false && !hasStatus && !hasRetcode {`,
	},

	// ---------- 缩略图编码（src/images/server）----------
	{
		name: "PNG 字节贴上 JPEG 标签（指鹿为马回到老路）",
		file: "src/images/server/window.go",
		old:  `	return buf.Bytes(), "image/png", nil`,
		new:  `	return buf.Bytes(), "image/jpeg", nil`,
	},

	// ---------- 审计闸门与已裁定白名单（src/memory/audit 与 cmd/sump-repair）----------
	// 七条各盯一处"改完照样编译、照样跑、看上去还说得通"的走法。
	// 隔离靠 cmd/sump-repair 与 audit 里那些单类场景：被删的检查必须是当时唯一能给出
	// 那个退出码的原因，别的类别全为空——所以删掉任何一条，只有它自己的用例翻脸。
	{
		name: "未裁定的命中不再破闸（novel 只打印不拦）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(novel) > 0 {`,
		new:  `	if false && len(novel) > 0 {`,
	},
	{
		name: "白名单过期不再破闸（清单与库脱钩当成功）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(stale) > 0 {`,
		new:  `	if false && len(stale) > 0 {`,
	},
	{
		name: "位置冲突不再破闸（键撞上而格子搬走也退出 0）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if len(conflicted) > 0 {`,
		new:  `	if false && len(conflicted) > 0 {`,
	},
	{
		name: "两个白名单开关不再互斥（谁赢由实现顺序决定）",
		file: "src/memory/cmd/sump-repair/main.go",
		old:  `	if *noAdjudicated && *adjudicatedPath != "" {`,
		new:  `	if false && *noAdjudicated && *adjudicatedPath != "" {`,
	},
	{
		name: "已裁定的位置不对账（列与探针被改也照样放行）",
		file: "src/memory/audit/adjudicated.go",
		old: `		if entry.Table == f.Table && entry.RowKey == f.RowKey &&
			entry.Column == f.Column && entry.Probe == f.Probe {`,
		new: `		if entry.Table == f.Table && entry.RowKey == f.RowKey {`,
	},
	{
		name: "行键列写死 rowid（清单给了别的键列也不认）",
		file: "src/memory/audit/adjudicated.go",
		old:  `		keyColumn, ok := keyColumnOf(cols, a.Table, a.Column)`,
		new:  `		keyColumn, ok := "rowid", true`,
	},
	{
		name: "已知凭据按出现次数计（同一值在清单里写两遍被数两次）",
		file: "src/memory/audit/audit.go",
		old:  `		if seen[k] || !strings.Contains(text, k) {`,
		new:  `		if !strings.Contains(text, k) {`,
	},
}

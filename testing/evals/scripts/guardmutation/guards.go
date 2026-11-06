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
		old:  `	rep.Remaining, err = verify(tx)`,
		new:  `	rep.Remaining, err = nil, nil`,
	},
	{
		name: "清理清单漏掉一列（那张表永远不被检）",
		file: "src/memory/repair/repair.go",
		old:  `		{"anchor", kText},`,
		new:  `		// anchor 是种子句，写死的，不用洗`,
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
}

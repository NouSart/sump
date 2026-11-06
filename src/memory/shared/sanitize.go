package shared

// 这里是共享库脱敏规则的**转发层**：规则本体在 redact 包，由所有写入路共用
// （Add / Attach / Publish / 一次性迁移）。留这个转发是为了老调用方与既有测试
// 不必跟着改名字，不是为了再写一套。

import "github.com/NouSart/sump/memory/redact"

// Sanitize 把一段自由文本压成"可以进共享库"的一句话：
// 去链接 / 路径 / 邮箱 / 密钥形状 / 手机号，压平换行，按 max 个字符截断。
// max <= 0 时只做脱敏、不截断。
func Sanitize(text string, max int) string { return redact.Sanitize(text, max) }

// Truncate 按**字符**（不是字节）截断，超出时补一个省略号。
func Truncate(s string, max int) string { return redact.Truncate(s, max) }

// SanitizeCapabilities 规范能力族清单：小写、只留标识符字符、去重、封顶 6 个。
// 能力族来自 reasoner 的确定性统计（服务名），不来自模型措辞。
func SanitizeCapabilities(in []string) []string { return redact.Capabilities(in) }

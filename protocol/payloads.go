package protocol

import "github.com/vmihailenco/msgpack/v5"

// Provide 描述服务对外提供的一个动作（“接受什么输入、有什么输出”）。
type Provide struct {
	Action string `msgpack:"action"`
	Input  string `msgpack:"input,omitempty"`
	Output string `msgpack:"output,omitempty"`
	// Tool 显式声明该动作是否作为 LLM 工具暴露给模型（默认 false）。
	// 工具名 = <service>__<action>；链机制保留动作与调试动作即便声明也不暴露（第二道闸）。
	Tool bool `msgpack:"tool,omitempty"`
}

// Setting 是服务的一个可设置项。
type Setting struct {
	Key     string `msgpack:"key"`
	Default string `msgpack:"default,omitempty"`
}

// RegisterPayload 是 register 请求的 payload：服务名片。
type RegisterPayload struct {
	Name        string    `msgpack:"name"`
	Addr        string    `msgpack:"addr"`
	Description string    `msgpack:"description,omitempty"`
	Provides    []Provide `msgpack:"provides,omitempty"`
	Settings    []Setting `msgpack:"settings,omitempty"`
	HeartbeatMS int64     `msgpack:"heartbeat_ms,omitempty"` // 心跳间隔（毫秒；0 = 未声明，中心用默认）
}

// ServiceCard 是 roster 中一个服务的公开部分。
type ServiceCard struct {
	Name        string    `msgpack:"name"`
	Addr        string    `msgpack:"addr"`
	Description string    `msgpack:"description,omitempty"`
	Provides    []Provide `msgpack:"provides,omitempty"`
	HeartbeatMS int64     `msgpack:"heartbeat_ms,omitempty"` // 心跳间隔（毫秒；离线判定用）
}

// Card 返回名片去掉私有部分（settings）后的公开视图。
func (r RegisterPayload) Card() ServiceCard {
	return ServiceCard{
		Name:        r.Name,
		Addr:        r.Addr,
		Description: r.Description,
		Provides:    r.Provides,
		HeartbeatMS: r.HeartbeatMS,
	}
}

// ResponsePayload 是通用响应的 payload。
type ResponsePayload struct {
	OK    bool               `msgpack:"ok"`
	Data  msgpack.RawMessage `msgpack:"data,omitempty"`
	Error string             `msgpack:"error,omitempty"`
}

// RosterPayload 是全局服务清单（register 响应与 roster 事件共用）。
type RosterPayload struct {
	Services []ServiceCard `msgpack:"services"`
	Revision int64         `msgpack:"revision"`
}

// JumpPayload 是一次跳转访问：目标服务上的动作 + 输入。
type JumpPayload struct {
	Action string             `msgpack:"action"`
	Input  msgpack.RawMessage `msgpack:"input,omitempty"`
}

// HeartbeatPayload 是报活 payload。
type HeartbeatPayload struct {
	Status string `msgpack:"status,omitempty"`
}

// ---------- 记忆往返（完全启动链） ----------

// TaskContext 是 LLM 服务的任务上下文：随“记忆往返”托管给记忆服务。
// Messages 是推理中的完整消息链；Boss 是原任务发起者（不可丢失，由记忆服务原样带回）。
type TaskContext struct {
	ConversationID string    `msgpack:"conversation_id,omitempty"`
	Messages       []Message `msgpack:"messages"`
	Boss           string    `msgpack:"boss,omitempty"`
}

// RecallPayload 是 recall（向记忆请求记忆）的 payload：把任务上下文交给记忆服务。
type RecallPayload struct {
	Context TaskContext `msgpack:"context"`
}

// ResumePayload 是 resume 的 payload：记忆服务组装好上下文后发回，
// 调用方收到来自记忆的消息时才“完全启动”（调用 LLM API）。
type ResumePayload struct {
	Context TaskContext `msgpack:"context"`
}

// StorePayload 是 store 的 payload：向记忆服务记一条对话消息。
type StorePayload struct {
	ConversationID string `msgpack:"conversation_id"`
	Role           string `msgpack:"role"`
	Content        string `msgpack:"content"`
}

// ---------- 任务链入口 ----------

// UserMessagePayload 是 user_message（链入口）的 payload：用户消息文本 + 图片引用。
type UserMessagePayload struct {
	Text           string   `msgpack:"text"`
	ConversationID string   `msgpack:"conversation_id,omitempty"`
	Images         []string `msgpack:"images,omitempty"` // 图片引用（images 服务中的 id）
}

// ---------- 交付与图片 ----------

// DeliverPayload 是 deliver（链终点 → boss）的 payload：最终文本 + 会话标识（boss 据此路由回复）。
type DeliverPayload struct {
	Text           string `msgpack:"text"`
	ConversationID string `msgpack:"conversation_id,omitempty"`
}

// ImageSavePayload 是 images.save 的 payload：URL 与 base64 二选一。
type ImageSavePayload struct {
	URL  string `msgpack:"url,omitempty"`
	Data string `msgpack:"data,omitempty"` // base64（不含 data: 前缀）
	Mime string `msgpack:"mime,omitempty"` // 可选，缺省时探测
}

// ImageSaveResult 是 images.save 的结果。
type ImageSaveResult struct {
	ID   string `msgpack:"id"`
	Mime string `msgpack:"mime"`
	Size int64  `msgpack:"size"`
}

// ImageFetchPayload 是 images.fetch 的 payload。
type ImageFetchPayload struct {
	ID string `msgpack:"id"`
}

// ImageFetchResult 是 images.fetch 的结果（存入时二进制，取时转 base64）。
type ImageFetchResult struct {
	ID   string `msgpack:"id"`
	Mime string `msgpack:"mime"`
	Data string `msgpack:"data"` // base64（不含 data: 前缀）
	Size int64  `msgpack:"size"`
}

// ---------- 设置中心：设置存取 ----------

// SettingView 是一个设置项的当前状态（设置中心对外呈现）。
type SettingView struct {
	Key        string `msgpack:"key"`
	Value      string `msgpack:"value"`      // 当前生效值（有覆盖值时为覆盖值，否则为默认值）
	Default    string `msgpack:"default"`    // 服务注册时声明的默认值
	Overridden bool   `msgpack:"overridden"` // 是否被设置中心覆盖
}

// ServiceSettings 是一个服务的设置清单（按声明顺序）。
type ServiceSettings struct {
	Service  string        `msgpack:"service"`
	Settings []SettingView `msgpack:"settings,omitempty"`
}

// SettingsListPayload 是 list_settings 的输入：可选按服务过滤（服务名）。
type SettingsListPayload struct {
	Service string `msgpack:"service,omitempty"`
}

// SettingsResult 是设置查询 / 变更的输出（list_settings / set_setting / reset_setting）：
// list 返回全部（或过滤后的）服务；set / reset 返回被改动的那个服务。
type SettingsResult struct {
	Services []ServiceSettings `msgpack:"services"`
}

// SetSettingPayload 是 set_setting 的输入：把某服务的某项设为某值（写覆盖值）。
type SetSettingPayload struct {
	Service string `msgpack:"service"`
	Key     string `msgpack:"key"`
	Value   string `msgpack:"value"`
}

// ResetSettingPayload 是 reset_setting 的输入：清除覆盖值，回落服务声明的默认值。
type ResetSettingPayload struct {
	Service string `msgpack:"service"`
	Key     string `msgpack:"key"`
}

// ConfigurePayload 是 configure 的输入：设置中心把生效值下发给所属服务
// （注册成功后全量逐键下发；set_setting / reset_setting 后单键下发）。
type ConfigurePayload struct {
	Key        string `msgpack:"key"`
	Value      string `msgpack:"value"`      // 生效值 = 覆盖值 ?? 声明默认值
	Default    string `msgpack:"default"`    // 服务声明的默认值
	Overridden bool   `msgpack:"overridden"` // 是否处于覆盖状态
}

// SettingResult 是一次下发中单个设置项的结果（settings 事件用）。
type SettingResult struct {
	Key   string `msgpack:"key"`
	Value string `msgpack:"value,omitempty"` // 成功应用后的生效值
	OK    bool   `msgpack:"ok"`
	Error string `msgpack:"error,omitempty"`
}

// SettingsEventPayload 是 settings 事件的载荷：中心广播某服务的设置变更与下发结果。
// Cause 复用触发的动作名（register / set_setting / reset_setting）。
type SettingsEventPayload struct {
	Cause   string          `msgpack:"cause"`
	Service string          `msgpack:"service"`
	Results []SettingResult `msgpack:"results,omitempty"`
}

// ---------- embedding（文本 → 向量） ----------

// EmbedPayload 是 embed 的输入：一批文本；Query 标记查询侧（服务按模型建议决定是否加指令前缀）。
type EmbedPayload struct {
	Texts []string `msgpack:"texts"`
	Query bool     `msgpack:"query,omitempty"`
}

// EmbedResult 是 embed 的输出：与 texts 一一对应的向量（L2 归一化）+ 维度 + 模型标识
// （模型标识用于判定已存向量是否失效）。向量在线路上是 float64（Python/JS 原生数字），
// Go 消费侧按需转 float32。
type EmbedResult struct {
	Vectors [][]float64 `msgpack:"vectors"`
	Dim     int         `msgpack:"dim"`
	Model   string      `msgpack:"model"`
}

// ---------- 长期记忆（memory） ----------

// RememberPayload 是 remember 的 payload：写入一条长期记忆条目。
type RememberPayload struct {
	Kind           string `msgpack:"kind"`                      // 分类（自由文本：偏好 / 身份 / 知识 / 事件……）
	Content        string `msgpack:"content"`                   // 条目正文
	Priority       int    `msgpack:"priority,omitempty"`        // >0 = 核心记忆（每次对话都注入，不看相关性）
	ConversationID string `msgpack:"conversation_id,omitempty"` // 可空 = 全局条目
}

// RememberResult 是 remember 的结果：条目 id 与是否新建（重复内容幂等跳过）。
type RememberResult struct {
	ID      int64 `msgpack:"id"`
	Created bool  `msgpack:"created"`
}

// ForgetPayload 是 forget 的 payload：软删一条长期记忆（不在 provides 里——不给 LLM 删记忆的按钮）。
type ForgetPayload struct {
	ID int64 `msgpack:"id"`
}

// ForgetResult 是 forget 的结果。
type ForgetResult struct {
	ID int64 `msgpack:"id"`
}

// ---------- 错误上报（服务 → 设置中心） ----------

// ReportPayload 是 report 的 payload：一条错误上报。
// 不含链路编号（当前没有链路记录者，记了也无处可查）；时间以中心收到为准。
type ReportPayload struct {
	Kind    string `msgpack:"kind,omitempty"` // 错误类别（上报方自定，如 fire / deliver / tool）
	Message string `msgpack:"message"`        // 错误简述
}

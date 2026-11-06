package tests

// callResult 是测试侧对一次原生 call 的转述：OK=false 时 Error 是原生错误文本，
// OK=true 时 Data 是 reply body。业务代码只走 SPv2，不再有 v1 响应载荷。
type callResult struct {
	OK    bool
	Data  []byte
	Error string
}

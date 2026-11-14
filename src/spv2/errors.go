package spv2

// 标准错误码。
const (
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodeConflict          = "conflict"
	CodeDeadlineExceeded  = "deadline_exceeded"
	CodeCancelled         = "cancelled"
	CodePermissionDenied  = "permission_denied"
	CodeResourceExhausted = "resource_exhausted"
	CodeUnavailable       = "unavailable"
	CodeInternal          = "internal"
	CodeStalled           = "stalled"
	CodeNotSupported      = "not_supported"
)

// RetryableByCode 是 spec §8 错误码表的唯一机器可读副本：
// 任何调用方想按码决定能不能重试，都应查这里，而不是各自猜。
func RetryableByCode(code string) bool {
	switch code {
	case CodeDeadlineExceeded, CodeResourceExhausted, CodeUnavailable, CodeInternal:
		return true
	default: // invalid_argument / not_found / conflict / cancelled / permission_denied / stalled / not_supported
		return false
	}
}

// ErrCode 返回一个 *Error。
func ErrCode(code, msg string, retryable bool, details map[string]any) *Error {
	return &Error{Code: code, Message: msg, Retryable: retryable, Details: details}
}

// IsCode 判断错误码。
func IsCode(err error, code string) bool {
	if e, ok := err.(*Error); ok {
		return e.Code == code
	}
	return false
}

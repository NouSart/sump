package spv2

import (
	"errors"
	"testing"
)

func TestErrorCode(t *testing.T) {
	e := ErrCode(CodePermissionDenied, "no", false, map[string]any{"x": 1})
	if !IsCode(e, CodePermissionDenied) {
		t.Fatal("IsCode failed")
	}
	if IsCode(errors.New("plain"), CodePermissionDenied) {
		t.Fatal("plain error must not match")
	}
	if e.Error() == "" {
		t.Fatal("Error() empty")
	}
	var nilErr *Error
	if nilErr.Error() != "" {
		t.Fatal("nil error should be empty")
	}
}

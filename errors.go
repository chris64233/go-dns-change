package dnschange

import (
	"errors"
	"fmt"
)

// ErrorCode 区分不同类别的失败，便于调用方按类别处理。
type ErrorCode string

const (
	// ErrValidation 名称/TTL/记录值/冲突等校验失败；不会创建任何修订。
	ErrValidation ErrorCode = "validation_error"
	// ErrRevision 基准号不匹配、修订不存在、区域不存在等修订号问题。
	ErrRevision ErrorCode = "revision_conflict"
	// ErrApproval 审批人资格、职责分离、审批数量不足等。
	ErrApproval ErrorCode = "approval_error"
	// ErrState 状态机非法迁移，如发布未批准的修订、发布已终结的修订。
	ErrState ErrorCode = "state_error"
	// ErrIdempotency 幂等键重放但请求载荷与首次不一致。
	ErrIdempotency ErrorCode = "idempotency_error"
)

// Error 携带错误类别与可读信息。
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// CodeOf 提取 ErrorCode；非本包错误返回空字符串。
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func validationError(format string, args ...any) error {
	return &Error{Code: ErrValidation, Message: fmt.Sprintf(format, args...)}
}

func revisionError(format string, args ...any) error {
	return &Error{Code: ErrRevision, Message: fmt.Sprintf(format, args...)}
}

func approvalError(format string, args ...any) error {
	return &Error{Code: ErrApproval, Message: fmt.Sprintf(format, args...)}
}

func stateError(format string, args ...any) error {
	return &Error{Code: ErrState, Message: fmt.Sprintf(format, args...)}
}

func idempotencyError(format string, args ...any) error {
	return &Error{Code: ErrIdempotency, Message: fmt.Sprintf(format, args...)}
}

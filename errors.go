package dnschange

import (
	"errors"
	"fmt"
)

// ErrorKind 区分错误的类别：校验、修订冲突、审批、状态、幂等、未找到。
type ErrorKind string

const (
	// KindValidation 校验错误：名称不规范、TTL 越界、记录冲突等。
	KindValidation ErrorKind = "validation"
	// KindRevision 修订冲突：基准修订号落后于当前头部。
	KindRevision ErrorKind = "revision_conflict"
	// KindApproval 审批错误：审批人无资格、职责分离冲突等。
	KindApproval ErrorKind = "approval"
	// KindState 状态错误：非法的状态迁移（如发布未批准的变更）。
	KindState ErrorKind = "state"
	// KindIdempotency 幂等错误：同一幂等键携带了不同的负载。
	KindIdempotency ErrorKind = "idempotency"
	// KindNotFound 区域、变更或修订不存在。
	KindNotFound ErrorKind = "not_found"
)

// Error 是服务返回的统一错误类型，携带类别与可选的明细。
type Error struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
	Issues  []string  `json:"issues,omitempty"` // 校验错误的逐项明细
}

func (e *Error) Error() string {
	if len(e.Issues) == 0 {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s (%d issues)", e.Kind, e.Message, len(e.Issues))
}

// IsKind 报告 err 是否为指定类别的服务错误。
func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

// IsValidation 报告 err 是否为校验错误。
func IsValidation(err error) bool { return IsKind(err, KindValidation) }

// IsRevisionConflict 报告 err 是否为修订冲突错误。
func IsRevisionConflict(err error) bool { return IsKind(err, KindRevision) }

// IsApproval 报告 err 是否为审批错误。
func IsApproval(err error) bool { return IsKind(err, KindApproval) }

// IsState 报告 err 是否为状态错误。
func IsState(err error) bool { return IsKind(err, KindState) }

// IsIdempotency 报告 err 是否为幂等错误。
func IsIdempotency(err error) bool { return IsKind(err, KindIdempotency) }

// IsNotFound 报告 err 是否为未找到错误。
func IsNotFound(err error) bool { return IsKind(err, KindNotFound) }

func errf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func validationErr(issues []string) *Error {
	return &Error{Kind: KindValidation, Message: "zone view validation failed", Issues: issues}
}

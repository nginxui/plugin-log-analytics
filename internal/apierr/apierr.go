// Package apierr defines the numbered errors of the plugin HTTP API. The JSON
// shape (scope, code, message, params) is the one the host used for the same
// handlers, so clients keep translating the codes they already know.
package apierr

import (
	"errors"
	"fmt"
	"strings"
)

// Scope creates errors that share a scope name.
type Scope struct {
	name string
}

// NewScope returns a scope for error definitions.
func NewScope(name string) *Scope {
	return &Scope{name: name}
}

// New defines an error with a fixed message.
func (s *Scope) New(code int32, message string) error {
	return &Error{Scope: s.name, Code: code, Message: message}
}

// Error is a coded error. Message may hold {0}, {1} placeholders that Params
// fill in.
type Error struct {
	Scope   string   `json:"scope,omitempty"`
	Code    int32    `json:"code"`
	Message string   `json:"message"`
	Params  []string `json:"params,omitempty"`
}

// Error renders the message with its parameters.
func (e *Error) Error() string {
	if len(e.Params) == 0 {
		return e.Message
	}
	msg := e.Message
	for index, param := range e.Params {
		msg = strings.Replace(msg, fmt.Sprintf("{%d}", index), param, 1)
	}
	return msg
}

// WithParams returns a copy of err with the parameters appended. An error that
// is not an *Error is returned unchanged.
func WithParams(err error, params ...string) error {
	var coded *Error
	if !errors.As(err, &coded) {
		return err
	}
	merged := make([]string, 0, len(coded.Params)+len(params))
	merged = append(merged, coded.Params...)
	merged = append(merged, params...)
	return &Error{Scope: coded.Scope, Code: coded.Code, Message: coded.Message, Params: merged}
}

// Package validation identifies the field that needs correction without making
// domain managers depend on a particular user interface.
package validation

import "fmt"

type Error struct {
	Field string
	Err   error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func At(field string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Field: field, Err: err}
}

func New(field, format string, args ...any) error {
	return At(field, fmt.Errorf(format, args...))
}

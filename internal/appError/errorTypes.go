package appError

import (
	"errors"
)

var (
	ErrReceiveMessage    = errors.New("failed to receive message")
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotFound          = errors.New("resource not found")
	ErrAlreadyExists     = errors.New("resource already exists")
	ErrInternalServer    = errors.New("internal server error")
	ErrTimeout           = errors.New("operation timeout")
	ErrUnavailable       = errors.New("service unavailable")
	ErrReadingHeader     = errors.New("error reading message header")
	ErrDecodingHeader    = errors.New("error decoding message header")
	ErrInvalidMgicNumber = errors.New("invalid magic number in message header")
	ErrReadingBody       = errors.New("error reading message body")
	ErrHandshakeFailed   = errors.New("handshake failed")
)

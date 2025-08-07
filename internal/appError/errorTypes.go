package appError

import (
	"errors"
)

var (
	ErrConnection = errors.New("connection error")
)

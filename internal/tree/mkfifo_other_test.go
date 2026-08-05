//go:build !unix

package tree

import "errors"

// mkfifo 在非 Unix 平台无对应概念，调用方据错误跳过该用例
func mkfifo(string) error { return errors.New("FIFO not supported on this platform") }

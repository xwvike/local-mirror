package tree

import "os"

func SyncFileData(f *os.File) error { return f.Sync() }

func syncQueuedData() error { return nil }

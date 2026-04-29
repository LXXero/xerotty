package glyphcache

import "unsafe"

func unsafePtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

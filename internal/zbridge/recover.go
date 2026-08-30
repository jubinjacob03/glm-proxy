package zbridge

import "runtime/debug"

func logPanic(name string, rec interface{}) {
	logErrorf("[panic] %s: %v\n%s", name, rec, printableASCII(string(debug.Stack())))
}

func recoverGoroutine(name string) {
	if rec := recover(); rec != nil {
		logPanic(name, rec)
	}
}

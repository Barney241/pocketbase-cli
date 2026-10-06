package cli

import "syscall"

const processNotDumpable = 0

func protectProcessMemory() {
	syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, processNotDumpable, 0)
}

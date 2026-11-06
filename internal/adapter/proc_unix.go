//go:build !windows

package adapter

// Unix 进程树清理（§8.5）：Setpgid 让子进程独立进程组，KillTree 用 -pgid 整组回收。
// 不做 Unix 可执行位判定（Windows 靠 PATHEXT，由 exec.LookPath 统一处理）。

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type procHandle struct {
	mu      sync.Mutex
	pgid    int
	cleanup func()
}

// rewriteCommand 在 Unix 上无需重写；.cmd → PS1 只属于 Windows。
func rewriteCommand(descriptor Descriptor, execPath string) (string, []string, func(), error) {
	_ = descriptor
	return execPath, nil, func() {}, nil
}

// prepareProc 让子进程成为新进程组组长。
func prepareProc(cmd *exec.Cmd, descriptor Descriptor, execPath string, cleanup func()) (*procHandle, error) {
	_ = descriptor
	_ = execPath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &procHandle{cleanup: cleanup}, nil
}

// attach 记录进程组 id（Start 之后调用）。
func (h *procHandle) attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("CLI 进程未启动")
	}
	h.mu.Lock()
	h.pgid = cmd.Process.Pid
	h.mu.Unlock()
	return nil
}

// killTree 先 SIGTERM 整组，紧接着 SIGKILL 整组兜底。
// 同步执行、不设延时定时器，避免 pid 复用误杀；整树回收优先，杜绝孤儿 CLI。
func (h *procHandle) killTree() {
	h.mu.Lock()
	pgid := h.pgid
	h.mu.Unlock()
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// close 释放平台资源。
func (h *procHandle) close() {
	if h.cleanup != nil {
		h.cleanup()
	}
}

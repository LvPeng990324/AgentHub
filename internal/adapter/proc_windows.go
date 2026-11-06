//go:build windows

package adapter

// Windows 进程树清理（§8.5）：Job Object + KILL_ON_JOB_CLOSE，CloseHandle(job) 即整树回收。
// 全部使用 golang.org/x/sys/windows 的纯 Go syscall 封装，不引入 CGO（C-2）。
// windows_special=✔ 的 .cmd shim 必须重写为 PS1，避免 cmd.exe %* 重分词打散多行 prompt。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type procHandle struct {
	mu      sync.Mutex
	job     windows.Handle
	cleanup func()
	closed  bool
}

// rewriteCommand 把 npm 安装的 .cmd shim 包装成 PowerShell 脚本调用。
// 设计规格为 CREATE_SUSPENDED → Assign → ResumeThread；os/exec 不暴露主线程句柄，
// 故这里采用「Start 后立即 AssignProcessToJobObject」：进程及其子进程同属 job，
// KILL_ON_JOB_CLOSE 同样保证整树回收，且不依赖任何 C 工具链。
func rewriteCommand(descriptor Descriptor, execPath string) (string, []string, func(), error) {
	if !descriptor.WindowsSpecial || !strings.HasSuffix(strings.ToLower(execPath), ".cmd") {
		return execPath, nil, func() {}, nil
	}
	quoted := strings.ReplaceAll(execPath, "'", "''")
	script := "$ErrorActionPreference = 'Continue'\n& '" + quoted + "' @args\n"
	file, err := os.CreateTemp("", "agenthub-*.ps1")
	if err != nil {
		return "", nil, func() {}, fmt.Errorf("创建 PS1 包装脚本失败: %w", err)
	}
	name := file.Name()
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", nil, func() {}, fmt.Errorf("写入 PS1 包装脚本失败: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, func() {}, fmt.Errorf("关闭 PS1 包装脚本失败: %w", err)
	}
	cleanup := func() { _ = os.Remove(name) }
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		pwsh = "powershell.exe"
	}
	return pwsh, []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", name}, cleanup, nil
}

// prepareProc 创建 Job Object 并设置 KILL_ON_JOB_CLOSE；同时隐藏 CLI 窗口。
func prepareProc(cmd *exec.Cmd, descriptor Descriptor, execPath string, cleanup func()) (*procHandle, error) {
	_ = descriptor
	_ = execPath
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 Job Object 失败: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("设置 Job Object 限制失败: %w", err)
	}
	// hideAgentWindow：Go 标准库在 HideWindow=true 时设置 STARTF_USESHOWWINDOW + SW_HIDE；
	// CREATE_NEW_CONSOLE 避免适配器所在控制台被 CLI 抢占，防止弹窗风暴。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_CONSOLE,
		HideWindow:    true,
	}
	return &procHandle{job: job, cleanup: cleanup}, nil
}

// attach 把已启动的进程加入 Job Object（Start 之后立即调用）。
func (h *procHandle) attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("CLI 进程未启动")
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("打开 CLI 进程句柄失败: %w", err)
	}
	defer func() { _ = windows.CloseHandle(processHandle) }()
	if err := windows.AssignProcessToJobObject(h.job, processHandle); err != nil {
		return fmt.Errorf("把 CLI 进程加入 Job Object 失败: %w", err)
	}
	return nil
}

// killTree 关闭 job 句柄即触发 KILL_ON_JOB_CLOSE 整树回收。
func (h *procHandle) killTree() {
	h.close()
}

// close 幂等释放 job 句柄与临时 PS1 脚本。
func (h *procHandle) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.job != 0 {
		_ = windows.CloseHandle(h.job)
		h.job = 0
	}
	if h.cleanup != nil {
		h.cleanup()
	}
}

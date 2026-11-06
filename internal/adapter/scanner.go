package adapter

// 公共 NDJSON scanner（§8.5）：S/A/AS/J-行式 四个解析器共用这一个构造函数，避免各族漂移。
// 初始 1MiB，单行上限 32MiB（codex thread/resume 会把整条线程塞进一行，10MiB 曾被打爆）。

import (
	"bufio"
	"io"
	"strings"
)

const (
	scannerInitialBuffer = 1 << 20  // 1 MiB
	scannerMaxTokenSize  = 32 << 20 // 32 MiB
)

// newAgentStreamScanner 构造去 CR 的行扫描器。
func newAgentStreamScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, scannerInitialBuffer), scannerMaxTokenSize)
	return scanner
}

// trimTrailingCR 去掉 Windows 行尾的 \r（在每行解析前调用）。
func trimTrailingCR(line string) string {
	return strings.TrimSuffix(line, "\r")
}

package adapter

// 回归用例（cwd 链路）：真实 ACP CLI（kimi）会校验 session/new 的 workspace root，
// 空 cwd 会被直接拒绝。假 CLI 默认不校验 cwd，所以必须显式断言协议层实际下发的字段。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// readDumpedParams 读取假 CLI 落盘的 session/new|session/resume params。
func readDumpedParams(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 ACP params 落盘失败（%s）: %v", path, err)
	}
	var dumped struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(raw, &dumped); err != nil {
		t.Fatalf("解析 ACP params 失败: %v", err)
	}
	if dumped.Params == nil {
		t.Fatal("ACP params 为空，测试自身失效")
	}
	return dumped.Params
}

// TestACPSessionNewCarriesWorkingDir ACP session/new 必须带**非空有效** cwd（kimi 校验 workspace root）。
func TestACPSessionNewCarriesWorkingDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "acp-params")
	t.Setenv("AGENTHUB_ACP_PARAMS_FILE", base)
	workingDir := t.TempDir() // 真实存在的目录

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeHermes)
	process := newHelperProcess(t, ctx, "acp", descriptor, nil, nil)
	session, err := NewACPParser().Parse(ctx, process, descriptor, "你好",
		BuildResume(descriptor, ""), ExecOptions{HandshakeTimeout: 5 * time.Second, Cwd: workingDir})
	if err != nil {
		t.Fatalf("ACP Parse 失败: %v", err)
	}
	if _, result := collectSession(t, session, 15*time.Second); result.Status != ResultCompleted {
		t.Fatalf("ACP 本轮应完成，实际 %s (%s)", result.Status, result.Error)
	}

	params := readDumpedParams(t, base+"-session_new.json")
	cwd, ok := params["cwd"].(string)
	if !ok || strings.TrimSpace(cwd) == "" {
		t.Fatalf("session/new 必须携带非空 cwd，实际 %#v（kimi 会拒绝空 workspace root）", params["cwd"])
	}
	if cwd != workingDir {
		t.Errorf("session/new 的 cwd 应为注入的 working_dir %q，实际 %q", workingDir, cwd)
	}
	if !filepath.IsAbs(cwd) {
		t.Errorf("cwd 应为绝对路径，实际 %q", cwd)
	}
}

// TestACPSessionParamsOmitEmptyCwd Cwd 为空时**不得**出现 "cwd": ""（宁可不下发该字段）。
func TestACPSessionParamsOmitEmptyCwd(t *testing.T) {
	base := filepath.Join(t.TempDir(), "acp-params")
	t.Setenv("AGENTHUB_ACP_PARAMS_FILE", base)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeHermes)

	// ① 新会话
	process := newHelperProcess(t, ctx, "acp", descriptor, nil, nil)
	session, err := NewACPParser().Parse(ctx, process, descriptor, "你好",
		BuildResume(descriptor, ""), ExecOptions{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ACP Parse 失败: %v", err)
	}
	if _, result := collectSession(t, session, 15*time.Second); result.Status != ResultCompleted {
		t.Fatalf("新会话应完成，实际 %s (%s)", result.Status, result.Error)
	}
	assertNoCwdKey(t, base+"-session_new.json", "session/new")

	// ② 会话续接（plan.Params 只含 sessionId，cwd 由 ExecOptions.Cwd 统一补）
	plan := BuildResume(descriptor, "ses-existing")
	if _, exists := plan.Params["cwd"]; exists {
		t.Error("BuildResume 不应硬编码 cwd（应由 proto_acp 基于 ExecOptions.Cwd 补齐）")
	}
	process2 := newHelperProcess(t, ctx, "acp", descriptor, nil, nil)
	session2, err := NewACPParser().Parse(ctx, process2, descriptor, "继续",
		plan, ExecOptions{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ACP Resume Parse 失败: %v", err)
	}
	if _, result := collectSession(t, session2, 15*time.Second); result.Status != ResultCompleted {
		t.Fatalf("续接应完成，实际 %s (%s)", result.Status, result.Error)
	}
	assertNoCwdKey(t, base+"-session_resume.json", "session/resume")
}

func assertNoCwdKey(t *testing.T, path string, label string) {
	t.Helper()
	params := readDumpedParams(t, path)
	if value, ok := params["cwd"]; ok {
		t.Errorf("%s 在 Cwd 为空时不得下发 cwd 字段，实际 %#v", label, value)
	}
}

// TestExpandLaunchArgsCwdPlaceholder cursor 的 `--workspace <cwd>` 必须展开成有效绝对路径，
// 绝不能产出 `--workspace ""`。
func TestExpandLaunchArgsCwdPlaceholder(t *testing.T) {
	cursor, ok := NewRegistry().Get(protocol.AgentTypeCursor)
	if !ok {
		t.Fatal("缺少 cursor 描述符")
	}
	workingDir := t.TempDir()

	expanded := expandLaunchArgs(cursor.LaunchArgs, workingDir)
	found := false
	for index, arg := range expanded {
		if arg != "--workspace" {
			continue
		}
		found = true
		if index+1 >= len(expanded) {
			t.Fatal("--workspace 缺少取值")
		}
		value := expanded[index+1]
		if strings.TrimSpace(value) == "" {
			t.Fatal("--workspace 的值不得为空串")
		}
		if value != workingDir {
			t.Errorf("--workspace 应为 %q，实际 %q", workingDir, value)
		}
		if !filepath.IsAbs(value) {
			t.Errorf("--workspace 应为绝对路径，实际 %q", value)
		}
	}
	if !found {
		t.Fatal("cursor 的 launch_args 中未找到 --workspace")
	}

	// 任何情况下都不能留下未展开的 <cwd> 占位符。
	for _, arg := range expandLaunchArgs(cursor.LaunchArgs, "") {
		if arg == "<cwd>" {
			t.Error("launch_args 中残留未展开的 <cwd> 占位符")
		}
	}
}

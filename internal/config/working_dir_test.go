package config

// 回归用例（cwd 链路）：adapter.working_dir 必须被规范化并在启动阶段校验存在性。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAdapterConfigFile(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, "adapter.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

// TestLoadAdapterConfigRejectsMissingWorkingDir working_dir 指向不存在的目录时，
// 适配器必须**启动即失败**并给出中文错误（否则 CLI 会在错误目录下静默跑）。
func TestLoadAdapterConfigRejectsMissingWorkingDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir")
	path := writeAdapterConfigFile(t, dir, fmt.Sprintf(
		`{"adapter":{"platform_ws_url":"ws://127.0.0.1:1/api/v1/ws/adapter","device_token":"t","working_dir":%q}}`,
		missing))

	_, err := LoadAdapterConfig(path)
	if err == nil {
		t.Fatal("working_dir 指向不存在的目录时必须返回错误")
	}
	message := err.Error()
	if !strings.Contains(message, "working_dir") || !strings.Contains(message, "不存在") {
		t.Errorf("错误文案应为中文且含 working_dir / 不存在，实际: %s", message)
	}
}

// TestLoadAdapterConfigWorkingDirNormalization 空值取进程 cwd、相对路径转绝对、显式目录原样。
func TestLoadAdapterConfigWorkingDirNormalization(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录失败: %v", err)
	}

	// ① 缺省 → 进程当前目录
	path := writeAdapterConfigFile(t, dir,
		`{"adapter":{"platform_ws_url":"ws://127.0.0.1:1/api/v1/ws/adapter","device_token":"t"}}`)
	cfg, err := LoadAdapterConfig(path)
	if err != nil {
		t.Fatalf("缺省 working_dir 不应报错: %v", err)
	}
	if cfg.WorkingDir != cwd {
		t.Errorf("缺省 working_dir 应回退到进程 cwd %q，实际 %q", cwd, cfg.WorkingDir)
	}
	if !filepath.IsAbs(cfg.WorkingDir) {
		t.Errorf("working_dir 应为绝对路径，实际 %q", cfg.WorkingDir)
	}

	// ② 相对路径 → 绝对
	path = writeAdapterConfigFile(t, dir,
		`{"adapter":{"platform_ws_url":"ws://127.0.0.1:1/api/v1/ws/adapter","device_token":"t","working_dir":"."}}`)
	cfg, err = LoadAdapterConfig(path)
	if err != nil {
		t.Fatalf("相对 working_dir 不应报错: %v", err)
	}
	if cfg.WorkingDir != cwd {
		t.Errorf("相对路径 \".\" 应解析为 %q，实际 %q", cwd, cfg.WorkingDir)
	}

	// ③ 显式绝对目录 → 原样保留
	explicit := t.TempDir()
	path = writeAdapterConfigFile(t, dir, fmt.Sprintf(
		`{"adapter":{"platform_ws_url":"ws://127.0.0.1:1/api/v1/ws/adapter","device_token":"t","working_dir":%q}}`,
		explicit))
	cfg, err = LoadAdapterConfig(path)
	if err != nil {
		t.Fatalf("显式 working_dir 不应报错: %v", err)
	}
	if cfg.WorkingDir != explicit {
		t.Errorf("显式 working_dir 应保持 %q，实际 %q", explicit, cfg.WorkingDir)
	}

	// WorkingDirectory() 对未 Load 的配置也要兜底非空（防止 ACP 传空 cwd）。
	empty := &AdapterConfig{}
	if got := empty.WorkingDirectory(); got == "" {
		t.Error("WorkingDirectory() 必须在未配置时回退到进程 cwd，绝不能返回空串")
	}
}

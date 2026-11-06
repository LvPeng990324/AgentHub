package adapter

// 设备 id 持久化（K6）：dev_<uuid4> 首启生成写入 ~/.agenthub/device_id，重启不变。
// 该文件只含设备标识，不含任何会话数据或恢复指针（§8.7：适配器完全无状态）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/agenthub/agenthub/internal/config"
)

const deviceIDPrefix = "dev_"

// LoadOrCreateDeviceID 读取持久化的 device_id；不存在或内容非法时生成并写入。
func LoadOrCreateDeviceID(cfg *config.AdapterConfig) (string, error) {
	path, err := cfg.DeviceIDFile()
	if err != nil {
		return "", err
	}
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		existing := strings.TrimSpace(string(data))
		if isValidDeviceID(existing) {
			return existing, nil
		}
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("读取设备 id 文件失败: %w", readErr)
	}

	deviceID := deviceIDPrefix + uuid.NewString()
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("创建设备 id 目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(deviceID+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入设备 id 失败: %w", err)
	}
	return deviceID, nil
}

// isValidDeviceID 校验持久化内容格式，避免旧版/损坏文件产生非法设备 id。
func isValidDeviceID(value string) bool {
	return strings.HasPrefix(value, deviceIDPrefix) && len(value) > len(deviceIDPrefix)
}

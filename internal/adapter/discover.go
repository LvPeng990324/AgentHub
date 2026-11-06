package adapter

// 自动发现（§8.4 / C-12）：四级探测 + 登录 shell TTL 缓存 + 2min 周期重探测 + 单向只增不减。
// OS 判定用 runtime.GOOS（WSL 内跑 Linux 版即报 linux，由 wsclient 上报）。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

const (
	shellProbeTimeout      = 5 * time.Second
	shellCacheTTL          = 30 * time.Minute
	unavailableAfterMisses = 3
)

// DiscoveredAgent 是上报给平台的快照条目（字段与 protocol.AgentSnapshot 一致）。
type DiscoveredAgent struct {
	Type        string
	DisplayName string
	Version     string
	ExecPath    string
	Status      string
}

type discoveredState struct {
	agent  DiscoveredAgent
	misses int
}

type shellCacheEntry struct {
	path     string
	cachedAt time.Time
}

// shellCache 以 PATH|SHELL|HOME 指纹为 key 的登录 shell 结果缓存（TTL 30min）。
type shellCache struct {
	mu          sync.Mutex
	fingerprint string
	entries     map[string]shellCacheEntry
}

func newShellCache() *shellCache {
	return &shellCache{entries: make(map[string]shellCacheEntry)}
}

func (c *shellCache) get(fingerprint string, command string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fingerprint != fingerprint {
		// 指纹变化即整体失效（调研 §7 结论 3）。
		c.fingerprint = fingerprint
		c.entries = make(map[string]shellCacheEntry)
		return "", false
	}
	entry, ok := c.entries[command]
	if !ok || time.Since(entry.cachedAt) > shellCacheTTL {
		return "", false
	}
	return entry.path, true
}

func (c *shellCache) put(fingerprint string, command string, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fingerprint != fingerprint {
		c.fingerprint = fingerprint
		c.entries = make(map[string]shellCacheEntry)
	}
	c.entries[command] = shellCacheEntry{path: path, cachedAt: time.Now()}
}

// shellFingerprint 是登录 shell 探测结果的缓存失效指纹。
func shellFingerprint() string {
	return os.Getenv("PATH") + "|" + os.Getenv("SHELL") + "|" + os.Getenv("HOME")
}

// Discoverer 周期探测本机 agent 并维护全量快照。
type Discoverer struct {
	cfg      *config.AdapterConfig
	registry *Registry
	probe    *VersionProbe
	cache    *shellCache

	mu     sync.RWMutex
	agents map[string]*discoveredState
}

// NewDiscoverer 构造自动发现器。
func NewDiscoverer(cfg *config.AdapterConfig, registry *Registry) *Discoverer {
	return &Discoverer{
		cfg:      cfg,
		registry: registry,
		probe:    NewVersionProbe(),
		cache:    newShellCache(),
		agents:   make(map[string]*discoveredState),
	}
}

// Probe 立即执行一次全量探测并返回最新快照（启动即探测，Q13）。
func (d *Discoverer) Probe(ctx context.Context) []DiscoveredAgent {
	found := make(map[string]bool)
	for _, descriptor := range d.registry.All() {
		execPath, ok := d.probePath(ctx, descriptor)
		if !ok {
			continue
		}
		found[descriptor.Type] = true
		agent := DiscoveredAgent{
			Type:        descriptor.Type,
			DisplayName: descriptor.DisplayName,
			Version:     d.probe.Run(ctx, execPath),
			ExecPath:    execPath,
			Status:      protocol.AgentStatusAvailable,
		}
		d.mu.Lock()
		d.agents[descriptor.Type] = &discoveredState{agent: agent, misses: 0}
		d.mu.Unlock()
	}

	d.mu.Lock()
	for agentType, state := range d.agents {
		if found[agentType] {
			continue
		}
		state.misses++
		state.agent.Status = protocol.AgentStatusUnavailable
		// 单向只增不减：单次未探到绝不删除；连续多个周期缺失后才从快照移除，
		// 平台据此把该 agent 置 online=0（仍保留 DB 行、remark 与 registered）。
		if state.misses >= unavailableAfterMisses {
			delete(d.agents, agentType)
			slog.Warn("agent 连续多个周期未探测到，从快照移除", "type", agentType, "misses", state.misses)
		}
	}
	snapshot := d.snapshotLocked()
	d.mu.Unlock()
	return snapshot
}

// Snapshot 返回当前全量快照的副本（按 type 排序）。
func (d *Discoverer) Snapshot() []DiscoveredAgent {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.snapshotLocked()
}

// snapshotLocked 必须在 d.mu 持有时调用。
func (d *Discoverer) snapshotLocked() []DiscoveredAgent {
	snapshot := make([]DiscoveredAgent, 0, len(d.agents))
	for _, state := range d.agents {
		snapshot = append(snapshot, state.agent)
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Type < snapshot[j].Type })
	return snapshot
}

// ExecPathFor 返回某类型 agent 的可执行路径（SessionManager 启动 CLI 用）。
func (d *Discoverer) ExecPathFor(agentType string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	state, ok := d.agents[agentType]
	if !ok {
		return "", false
	}
	return state.agent.ExecPath, state.agent.ExecPath != ""
}

// Loop 按周期重探测；每次探测结束调用 onChange（由 WSClient 重发全量快照对账）。
func (d *Discoverer) Loop(ctx context.Context, period time.Duration, onChange func([]DiscoveredAgent)) {
	if period <= 0 {
		period = time.Duration(config.DefaultDiscoverPeriodSeconds) * time.Second
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot := d.Probe(ctx)
			if onChange != nil {
				onChange(snapshot)
			}
		}
	}
}

// ProbeOne 对单个描述符执行四级探测，返回可执行路径（供单测与诊断）。
func (d *Discoverer) ProbeOne(ctx context.Context, descriptor Descriptor) (string, bool) {
	return d.probePath(ctx, descriptor)
}

// probePath 实现 §8.4 四级探测。
func (d *Discoverer) probePath(ctx context.Context, descriptor Descriptor) (string, bool) {
	// ① PATH 覆盖：配置文件 agent_paths 优先，其次 AGENTHUB_<TYPE>_PATH 环境变量。
	if configured := strings.TrimSpace(d.cfg.AgentPaths[descriptor.Type]); configured != "" {
		if pathExists(configured) {
			return configured, true
		}
		slog.Warn("PATH 覆盖指定的可执行文件不存在，继续回退探测",
			"type", descriptor.Type, "configured_path", configured)
	}
	if key := strings.TrimSpace(descriptor.PathEnvKey); key != "" {
		if fromEnv := strings.TrimSpace(os.Getenv(key)); fromEnv != "" && pathExists(fromEnv) {
			return fromEnv, true
		}
	}

	// ② exec.LookPath：Windows 由 Go 标准库按 PATHEXT 判定，绝不依赖 Unix 可执行位。
	if found, err := exec.LookPath(descriptor.DefaultCommand); err == nil && found != "" {
		return found, true
	}

	// ③ 登录 shell 回退（TTL 30min 缓存 + PATH/SHELL/HOME 指纹失效）。
	if found, ok := d.shellLookup(ctx, descriptor.DefaultCommand); ok {
		return found, true
	}

	// ④ ExtraProbePaths：逐个 os.Stat 存在性检查（如 codex Desktop app bundle）。
	for _, raw := range descriptor.ExtraProbePaths {
		candidate := expandUserPath(raw)
		if candidate != "" && pathExists(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// shellLookup 用登录 shell 的 `command -v` 找命令，5s 超时且结果按指纹缓存。
func (d *Discoverer) shellLookup(ctx context.Context, command string) (string, bool) {
	if runtime.GOOS == "windows" {
		return "", false
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		return "", false
	}
	fingerprint := shellFingerprint()
	if cached, ok := d.cache.get(fingerprint, command); ok {
		return cached, true
	}
	probeCtx, cancel := context.WithTimeout(ctx, shellProbeTimeout)
	defer cancel()
	// command 来自静态描述符表，不是用户输入；仍做单引号转义以防意外字符。
	script := fmt.Sprintf("command -v '%s'", strings.ReplaceAll(command, "'", "'\\''"))
	output, err := exec.CommandContext(probeCtx, shell, "-lic", script).Output()
	if err != nil {
		return "", false
	}
	found := strings.TrimSpace(string(output))
	if found == "" || !pathExists(found) {
		return "", false
	}
	d.cache.put(fingerprint, command, found)
	return found, true
}

// pathExists 检查路径存在且不是目录。
func pathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// expandUserPath 展开以 ~ 开头的额外探测路径。
func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		remainder := strings.TrimPrefix(strings.TrimPrefix(path, "~/"), "~\\")
		return filepath.Join(home, remainder)
	}
	return path
}

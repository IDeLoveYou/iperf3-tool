package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Hub 保存网页中的多个独立测试标签。每个标签都有自己的 Manager，因此
// 可以同时运行不同服务端、端口或协议的 iperf3 测试，数据不会相互覆盖。
type Hub struct {
	mu        sync.RWMutex
	tests     map[string]*Manager
	starting  map[Config]*Manager
	replacing map[string]*Manager
	order     []string
	nextID    int
}

type TestInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Config   Config `json:"config"`
}

// DeleteResult 告诉前端删除后应切换到哪个标签。删除最后一个标签后列表为空，
// 网页回到“暂无监测数据”的初始配置界面。
type DeleteResult struct {
	DeletedID string     `json:"deletedId"`
	NextID    string     `json:"nextId"`
	Tests     []TestInfo `json:"tests"`
}

// StartResult 是“开始监测即新建标签”的原子操作结果。前端一次请求即可拿到
// 新标签、初始运行快照和完整标签列表，不需要先创建再启动两个独立步骤。
type StartResult struct {
	Test     TestInfo   `json:"test"`
	Snapshot Snapshot   `json:"snapshot"`
	Tests    []TestInfo `json:"tests"`
}

func NewHub() *Hub {
	return &Hub{
		tests:     make(map[string]*Manager),
		starting:  make(map[Config]*Manager),
		replacing: make(map[string]*Manager),
	}
}

// CreateAndStart 先校验配置，再创建并启动一个全新的测试标签。校验失败时
// 不会留下空标签；因此前端的“开始监测”可以安全地直接表达“新建并启动”。
func (h *Hub) CreateAndStart(ctx context.Context, config Config) (StartResult, error) {
	config = normalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return StartResult{}, err
	}

	// 重复判断覆盖所有已有标签，不只比较网页当前选中的标签。
	// starting 同时作为启动中配置的预留表，防止两个并发请求绕过检查。
	manager := NewManager(config)
	h.mu.Lock()
	if index, exists := h.duplicateIndexLocked(config, ""); exists {
		h.mu.Unlock()
		return StartResult{}, duplicateConfigError(index)
	}
	if _, exists := h.starting[config]; exists {
		h.mu.Unlock()
		return StartResult{}, errors.New("相同配置的监测正在启动，请勿重复提交")
	}
	h.starting[config] = manager
	h.mu.Unlock()
	defer h.releaseCandidate("", config, manager)

	// 先在标签列表之外启动。只有 iperf3 产生第一条有效区间后才写入 Hub，
	// 避免控制连接成功后立即报错的任务在下拉框里留下无效标签。
	if err := startAndWaitReady(ctx, manager, config); err != nil {
		manager.Close()
		return StartResult{}, err
	}

	h.mu.Lock()
	if h.starting[config] != manager {
		h.mu.Unlock()
		manager.Close()
		return StartResult{}, errors.New("监测启动已取消")
	}
	h.nextID++
	id := fmt.Sprintf("test-%d", h.nextID)
	h.tests[id] = manager
	h.order = append(h.order, id)
	delete(h.starting, config)
	h.mu.Unlock()
	return h.startResult(id, manager), nil
}

// Delete 先从 Hub 中摘除标签，再关闭其 Manager。Close 会取消运行中的
// iperf3 上下文，并防止与删除请求并发到达的启动请求留下孤立进程。
func (h *Hub) Delete(id string) (DeleteResult, error) {
	h.mu.Lock()
	manager, exists := h.tests[id]
	if !exists {
		h.mu.Unlock()
		return DeleteResult{}, errors.New("测试标签不存在")
	}
	deleteIndex := -1
	for index, candidate := range h.order {
		if candidate == id {
			deleteIndex = index
			h.order = append(h.order[:index], h.order[index+1:]...)
			break
		}
	}
	delete(h.tests, id)
	// 删除请求如果与“临时启动新配置”并发，同时取消候选任务，
	// 防止已删除标签在候选任务就绪后被重新写回。
	replacement := h.replacing[id]
	if replacement != nil {
		delete(h.replacing, id)
		if h.starting[replacement.Config()] == replacement {
			delete(h.starting, replacement.Config())
		}
	}
	nextID := ""
	if len(h.order) > 0 {
		if deleteIndex < 0 {
			deleteIndex = 0
		}
		if deleteIndex >= len(h.order) {
			deleteIndex = len(h.order) - 1
		}
		nextID = h.order[deleteIndex]
	}
	h.mu.Unlock()

	if replacement != nil {
		replacement.Close()
	}
	manager.Close()
	return DeleteResult{DeletedID: id, NextID: nextID, Tests: h.List()}, nil
}

func (h *Hub) Get(id string) (*Manager, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if id == "" && len(h.order) > 0 {
		id = h.order[0]
	}
	manager, ok := h.tests[id]
	if !ok {
		return nil, errors.New("测试标签不存在")
	}
	return manager, nil
}

// ReplaceAndStart 为已停止或错误标签启动一个候选 Manager。候选任务
// 收到首条有效数据后才原子替换旧 Manager；配置或 iperf3 启动失败时，
// 原配置、原状态和原历史都不受影响。
func (h *Hub) ReplaceAndStart(ctx context.Context, id string, config Config) (StartResult, error) {
	config = normalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return StartResult{}, err
	}
	candidate := NewManager(config)

	h.mu.Lock()
	previous, exists := h.tests[id]
	if !exists {
		h.mu.Unlock()
		return StartResult{}, errors.New("测试标签不存在")
	}
	state := previous.Snapshot().State
	if state == "running" || state == "stopping" {
		h.mu.Unlock()
		return StartResult{}, errors.New("监测运行中不能修改配置，请先停止当前监测")
	}
	if index, duplicate := h.duplicateIndexLocked(config, id); duplicate {
		h.mu.Unlock()
		return StartResult{}, duplicateConfigError(index)
	}
	if _, exists := h.replacing[id]; exists {
		h.mu.Unlock()
		return StartResult{}, errors.New("当前标签的新配置正在启动，请勿重复提交")
	}
	if _, exists := h.starting[config]; exists {
		h.mu.Unlock()
		return StartResult{}, errors.New("相同配置的监测正在启动，请勿重复提交")
	}
	h.starting[config] = candidate
	h.replacing[id] = candidate
	h.mu.Unlock()
	defer h.releaseCandidate(id, config, candidate)

	if err := startAndWaitReady(ctx, candidate, config); err != nil {
		candidate.Close()
		return StartResult{}, err
	}

	h.mu.Lock()
	if h.tests[id] != previous || h.replacing[id] != candidate || h.starting[config] != candidate {
		h.mu.Unlock()
		candidate.Close()
		return StartResult{}, errors.New("测试标签已被删除或替换，未保存新配置")
	}
	h.tests[id] = candidate
	delete(h.replacing, id)
	delete(h.starting, config)
	h.mu.Unlock()

	// 其他浏览器可能仍订阅旧 Manager。替换事件会让它们立即改连
	// 新 Manager；retire 则防止旧对象之后被误启动。
	previous.publish("replace", candidate.Snapshot())
	previous.retire()
	return h.startResult(id, candidate), nil
}

// startAndWaitReady 是新建标签和修改已有标签共用的启动门禁。
// 只有首条有效区间到达且任务仍在运行时，调用方才能保存配置。
func startAndWaitReady(ctx context.Context, manager *Manager, config Config) error {
	if err := manager.Start(config); err != nil {
		return err
	}
	firstIntervalSeconds := config.IntervalSeconds
	if firstIntervalSeconds > float64(config.ProbeDuration) {
		firstIntervalSeconds = float64(config.ProbeDuration)
	}
	readyTimeout := time.Duration((float64(config.OmitSeconds) + firstIntervalSeconds + 5) * float64(time.Second))
	if readyTimeout < 15*time.Second {
		readyTimeout = 15 * time.Second
	}
	readyContext, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := manager.WaitReady(readyContext); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("等待首条有效监测数据超时，未保存配置")
		}
		return fmt.Errorf("监测启动失败：%w", err)
	}
	snapshot := manager.Snapshot()
	if snapshot.State != "running" {
		message := snapshot.LastError
		if message == "" {
			message = "iperf3 在启动阶段已结束"
		}
		return errors.New(message)
	}
	return nil
}

func (h *Hub) duplicateIndexLocked(config Config, excludedID string) (int, bool) {
	for index, id := range h.order {
		if id != excludedID && h.tests[id].Config() == config {
			return index, true
		}
	}
	return 0, false
}

func duplicateConfigError(index int) error {
	return fmt.Errorf("配置已存在于“测试 %d”标签页，请切换到该标签页", index)
}

func (h *Hub) releaseCandidate(id string, config Config, manager *Manager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.starting[config] == manager {
		delete(h.starting, config)
	}
	if id != "" && h.replacing[id] == manager {
		delete(h.replacing, id)
	}
}

func (h *Hub) startResult(id string, manager *Manager) StartResult {
	tests := h.List()
	var info TestInfo
	for _, test := range tests {
		if test.ID == id {
			info = test
			break
		}
	}
	return StartResult{Test: info, Snapshot: manager.Snapshot(), Tests: tests}
}

func (h *Hub) List() []TestInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]TestInfo, 0, len(h.order))
	for index, id := range h.order {
		manager := h.tests[id]
		snapshot := manager.Snapshot()
		result = append(result, TestInfo{
			ID: id, Name: fmt.Sprintf("测试 %d", index), State: snapshot.State,
			Host: snapshot.Config.Host, Port: snapshot.Config.Port, Protocol: snapshot.Config.Protocol,
			Config: snapshot.Config,
		})
	}
	return result
}

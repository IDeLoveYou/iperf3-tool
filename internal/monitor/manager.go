package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxHistory = 300

// Manager 管理单个测试标签的监测任务。同一标签只允许运行一个任务；多个
// Manager 可由 Hub 并行调度，因此不同端口的测试不会互相覆盖状态和历史。
type Manager struct {
	mu              sync.RWMutex
	config          Config
	state           string
	sessionID       string
	startedAt       *time.Time
	updatedAt       time.Time
	probe           int
	pid             int
	connection      *ConnectionInfo
	version         string
	systemInfo      string
	latest          *Sample
	uploadLatest    *Sample
	downloadLatest  *Sample
	summary         *Summary
	uploadSummary   *Summary
	downloadSummary *Summary
	overallUpload   Summary
	overallDownload Summary
	history         []Sample
	lastError       string
	lastLog         string
	cancel          context.CancelFunc
	closed          bool
	ready           chan error
	readyDone       bool
	subscribers     map[chan Event]struct{}
}

func NewManager(config Config) *Manager {
	return &Manager{config: config, state: "idle", updatedAt: time.Now(), subscribers: make(map[chan Event]struct{})}
}

func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func ValidateConfig(c Config) error {
	c = normalizeConfig(c)
	if strings.TrimSpace(c.Host) == "" || len(c.Host) > 253 {
		return errors.New("服务端地址不能为空且不能超过 253 个字符")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("端口必须在 1 到 65535 之间")
	}
	if strings.TrimSpace(c.Bitrate) == "" || len(c.Bitrate) > 24 {
		return errors.New("目标速率不能为空，例如 10M、50M 或 1G")
	}
	if c.IntervalSeconds < 0.1 || c.IntervalSeconds > 60 {
		return errors.New("统计间隔必须在 0.1 到 60 秒之间")
	}
	if c.ProbeDuration < 1 || c.ProbeDuration > 3600 {
		return errors.New("单次探测时长必须在 1 到 3600 秒之间")
	}
	if c.PacketLength < 0 || c.PacketLength > 65507 {
		return errors.New("UDP 包长度必须在 0 到 65507 之间，0 表示使用 iperf3 默认值")
	}
	if c.ParallelStreams < 1 || c.ParallelStreams > 32 {
		return errors.New("并行流数量必须在 1 到 32 之间")
	}
	if c.OmitSeconds < 0 || c.OmitSeconds >= c.ProbeDuration {
		return errors.New("预热忽略时长必须大于等于 0 且小于单次探测时长")
	}
	if strings.TrimSpace(c.BinaryPath) == "" {
		return errors.New("iperf3 可执行文件不能为空")
	}
	if c.Protocol != "udp" && c.Protocol != "tcp" {
		return errors.New("检测协议必须是 UDP 或 TCP")
	}
	return nil
}

func (m *Manager) Start(config Config) error {
	// 连续探测是工具的固定运行方式，不再由网页或 API 调用方关闭。
	config = normalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("测试标签已删除")
	}
	if m.state == "running" || m.state == "stopping" {
		m.mu.Unlock()
		return errors.New("已有监测任务正在运行")
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	m.config, m.state, m.sessionID, m.startedAt = config, "running", newSessionID(), &now
	m.updatedAt, m.probe, m.pid = now, 0, 0
	m.connection, m.latest, m.uploadLatest, m.downloadLatest = nil, nil, nil, nil
	m.summary, m.uploadSummary, m.downloadSummary, m.history = nil, nil, nil, nil
	m.overallUpload, m.overallDownload = Summary{Protocol: config.Protocol}, Summary{Protocol: config.Protocol}
	m.version, m.systemInfo, m.lastError, m.lastLog = "", "", "", ""
	m.ready, m.readyDone = make(chan error, 1), false
	m.cancel = cancel
	m.mu.Unlock()

	m.publish("state", m.Snapshot())
	go m.run(ctx, config)
	return nil
}

// normalizeConfig 将所有入口收到的配置转换为同一种保存和比较形式。
// TCP 不使用目标速率与 UDP 包长度，因此固定为网页默认值，避免两个实际完全
// 相同的 TCP 测试仅因不可编辑的 UDP 参数不同而绕过重复配置检查。
func normalizeConfig(config Config) Config {
	config.Host = strings.TrimSpace(config.Host)
	config.Bitrate = strings.TrimSpace(config.Bitrate)
	config.BinaryPath = strings.TrimSpace(config.BinaryPath)
	config.Continuous = true
	if config.Protocol == "tcp" {
		config.Bitrate = "1M"
		config.PacketLength = 256
	}
	return config
}

// WaitReady 等待当前协议产生第一条有效区间。只有控制连接
// 而没有实际测试数据不算启动成功；Hub 仅在该方法成功后公开标签。
func (m *Manager) WaitReady(ctx context.Context) error {
	m.mu.RLock()
	ready := m.ready
	m.mu.RUnlock()
	if ready == nil {
		return errors.New("监测任务尚未启动")
	}
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.state != "running" {
		m.mu.Unlock()
		return errors.New("当前没有正在运行的监测任务")
	}
	m.state = "stopping"
	m.touchLocked()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.publish("state", m.Snapshot())
	return nil
}

// Close 永久关闭当前测试标签。与普通 Stop 不同，Close 还会阻止已经取得
// Manager 引用的并发请求再次启动任务，确保标签从 Hub 删除后不会残留进程。
func (m *Manager) Close() {
	m.retire()
	m.publish("state", m.Snapshot())
}

// retire 用于将 Manager 永久退役，但不发布新状态。替换配置时，
// Hub 会先向旧订阅发送 replace 事件，再用本方法防止旧状态覆盖新快照。
func (m *Manager) retire() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	if m.state == "running" {
		m.state = "stopping"
		m.touchLocked()
	}
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.signalReady(context.Canceled)
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotLocked()
}

func (m *Manager) snapshotLocked() Snapshot {
	return Snapshot{
		State: m.state, SessionID: m.sessionID, StartedAt: cloneTime(m.startedAt), UpdatedAt: m.updatedAt,
		Probe: m.probe, PID: m.pid, Config: m.config, Connection: cloneConnection(m.connection),
		IperfVersion: m.version, SystemInfo: m.systemInfo, Latest: cloneSample(m.latest),
		UploadLatest: cloneSample(m.uploadLatest), DownloadLatest: cloneSample(m.downloadLatest),
		Summary: cloneSummary(m.summary), UploadSummary: cloneSummary(m.uploadSummary), DownloadSummary: cloneSummary(m.downloadSummary),
		OverallUpload: overallSummaryLocked(m.overallUpload), OverallDownload: overallSummaryLocked(m.overallDownload),
		History:   append([]Sample(nil), m.history...),
		LastError: m.lastError, LastLog: m.lastLog,
	}
}

// Subscribe 返回独立缓冲区，单个浏览器卡顿时不会阻塞 iperf3 解析循环。
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 128)
	m.mu.Lock()
	m.subscribers[ch] = struct{}{}
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	ch <- Event{Type: "snapshot", Data: snapshot}

	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subscribers, ch)
			close(ch)
			m.mu.Unlock()
		})
	}
	return ch, closeFn
}

func (m *Manager) run(ctx context.Context, config Config) {
	for {
		m.mu.Lock()
		m.probe++
		probeNumber := m.probe
		m.mu.Unlock()

		err := m.runProbe(ctx, config, probeNumber)
		if err != nil && !errors.Is(err, context.Canceled) {
			m.mu.Lock()
			message := err.Error()
			// iperf3 的退出码通常只有 "exit status 1"，真正原因位于 stderr。
			// 合并最后一条 stderr 后，网页可以直接展示服务端忙、参数不兼容
			// 或连接中断等具体原因。
			if m.lastLog != "" && !strings.Contains(message, m.lastLog) {
				message += "；iperf3：" + m.lastLog
			}
			m.state, m.lastError = "error", message
			m.touchLocked()
			m.mu.Unlock()
			// 先保存完整错误，再通知正在等待启动结果的 Hub。
			// 这样新建或替换失败时的 Message 也能包含 stderr 详情。
			m.signalReady(errors.New(message))
			m.publish("error", m.Snapshot())
			return
		}
		if !config.Continuous || ctx.Err() != nil {
			if ctx.Err() != nil {
				m.signalReady(ctx.Err())
			} else {
				m.signalReady(errors.New("监测在建立连接前结束"))
			}
			m.mu.Lock()
			if m.state != "error" {
				m.state = "idle"
			}
			m.cancel = nil
			m.touchLocked()
			m.mu.Unlock()
			m.publish("state", m.Snapshot())
			return
		}
		select {
		case <-ctx.Done():
			m.finish()
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (m *Manager) finish() {
	m.signalReady(context.Canceled)
	m.mu.Lock()
	if m.state != "error" {
		m.state = "idle"
	}
	m.cancel = nil
	m.touchLocked()
	m.mu.Unlock()
	m.publish("state", m.Snapshot())
}

func (m *Manager) appendSample(sample Sample) {
	m.mu.Lock()
	// 总体丢包率基于每个已确认区间的丢包数和总包数累加，不能对区间百分比做简单平均。
	addOverallLossLocked(&m.overallUpload, &m.overallDownload, sample, 1)
	m.history = append(m.history, sample)
	if len(m.history) > maxHistory {
		m.history = m.history[len(m.history)-maxHistory:]
	}
	// 服务端文本有时包含一个收尾区间；它只有丢包/jitter，没有客户端发送
	// 字节和发包数，不能把它误当成“最新发送速率”卡片的数据来源。
	if sample.Source != "server" || m.latest == nil {
		m.latest = cloneSample(&sample)
	}
	if sample.Direction == "download" {
		if sample.Source != "server" || m.downloadLatest == nil {
			m.downloadLatest = cloneSample(&sample)
		}
	} else if sample.Direction == "upload" {
		if sample.Source != "server" || m.uploadLatest == nil {
			m.uploadLatest = cloneSample(&sample)
		}
	}
	m.touchLocked()
	m.mu.Unlock()
	m.signalReady(nil)
	m.publish("sample", sample)
	m.publishOverall()
}

func (m *Manager) updateSample(id string, loss, total int64, percent, jitter float64, outOfOrder int64, source string) {
	m.mu.Lock()
	var changed *Sample
	for i := range m.history {
		if m.history[i].ID != id {
			continue
		}
		// 同一个区间可能先以 live 状态到达，随后被服务端结果确认。
		// 先撤销旧值再加入新值，避免实时更新造成重复累计。
		addOverallLossLocked(&m.overallUpload, &m.overallDownload, m.history[i], -1)
		m.history[i].LostPackets, m.history[i].TotalPackets = int64Ptr(loss), int64Ptr(total)
		m.history[i].LossPercent, m.history[i].JitterMs = float64Ptr(percent), float64Ptr(jitter)
		m.history[i].OutOfOrder, m.history[i].Source, m.history[i].Status = int64Ptr(outOfOrder), source, "confirmed"
		addOverallLossLocked(&m.overallUpload, &m.overallDownload, m.history[i], 1)
		changed = cloneSample(&m.history[i])
		if m.latest != nil && m.latest.ID == id {
			m.latest = cloneSample(&m.history[i])
		}
		if m.history[i].Direction == "download" {
			m.downloadLatest = cloneSample(&m.history[i])
		} else {
			m.uploadLatest = cloneSample(&m.history[i])
		}
		break
	}
	m.touchLocked()
	m.mu.Unlock()
	if changed != nil {
		m.publish("sample", *changed)
		m.publishOverall()
	}
}

func (m *Manager) setStart(data startData) {
	m.mu.Lock()
	if len(data.Connected) > 0 {
		c := data.Connected[0]
		m.connection = &ConnectionInfo{LocalHost: c.LocalHost, LocalPort: c.LocalPort, RemoteHost: c.RemoteHost, RemotePort: c.RemotePort}
	}
	m.version, m.systemInfo = data.Version, data.SystemInfo
	m.touchLocked()
	m.mu.Unlock()
	m.publish("start", m.Snapshot())
}

func (m *Manager) signalReady(err error) {
	m.mu.Lock()
	if m.ready == nil || m.readyDone {
		m.mu.Unlock()
		return
	}
	m.readyDone = true
	ready := m.ready
	m.mu.Unlock()
	ready <- err
}

func (m *Manager) setDirectionSummary(protocol, direction string, summary Summary) {
	m.mu.Lock()
	summary.Protocol = protocol
	// --get-server-output 通常先于 end 事件到达。end 提供准确的客户端
	// 字节/吞吐/CPU，而服务端文本提供准确的丢包；两者需要合并保存。
	var previous *Summary
	if direction == "download" {
		previous = m.downloadSummary
	} else {
		previous = m.uploadSummary
	}
	if previous != nil && previous.Confirmed && !summary.Confirmed {
		summary.LostPackets = previous.LostPackets
		summary.Packets = previous.Packets
		summary.LossPercent = previous.LossPercent
		summary.JitterMs = previous.JitterMs
		summary.OutOfOrder = previous.OutOfOrder
		summary.Confirmed = true
	}
	if direction == "download" {
		m.downloadSummary = cloneSummary(&summary)
	} else {
		m.uploadSummary = cloneSummary(&summary)
		m.summary = cloneSummary(&summary) // 兼容旧字段：代表上行汇总。
	}
	m.touchLocked()
	overall := overallForDirectionLocked(m.overallUpload, m.overallDownload, direction)
	m.mu.Unlock()
	m.publish("summary", struct {
		Direction string `json:"direction"`
		Summary
		Overall *Summary `json:"overall,omitempty"`
	}{Direction: direction, Summary: summary, Overall: overall})
}

// addOverallLossLocked 累加已确认的 UDP 区间。它只在持有 m.mu 时调用。
// 总体丢包率按“累计丢失包 / 累计总包”计算，结果比平均各区间百分比更准确。
func addOverallLossLocked(upload, download *Summary, sample Sample, sign int64) {
	if sample.Protocol != "udp" || sample.Status != "confirmed" || sample.LostPackets == nil || sample.TotalPackets == nil {
		return
	}
	target := upload
	if sample.Direction == "download" {
		target = download
	} else if sample.Direction != "upload" {
		return
	}
	target.LostPackets += sign * *sample.LostPackets
	target.Packets += sign * *sample.TotalPackets
	if target.Packets <= 0 {
		*target = Summary{Protocol: "udp"}
		return
	}
	target.LossPercent = float64(target.LostPackets) * 100 / float64(target.Packets)
	target.Confirmed = true
}

func overallSummaryLocked(summary Summary) *Summary {
	if !summary.Confirmed || summary.Packets <= 0 {
		return nil
	}
	result := summary
	return &result
}

func overallForDirectionLocked(upload, download Summary, direction string) *Summary {
	if direction == "download" {
		return overallSummaryLocked(download)
	}
	return overallSummaryLocked(upload)
}

// publishOverall 将累计结果单独推送给网页，让总体丢包率在当前探测区间
// 被服务端确认后立即刷新，而不必等待下一次完整探测结束。
func (m *Manager) publishOverall() {
	m.mu.RLock()
	data := struct {
		Upload   *Summary `json:"upload"`
		Download *Summary `json:"download"`
	}{
		Upload: overallSummaryLocked(m.overallUpload), Download: overallSummaryLocked(m.overallDownload),
	}
	m.mu.RUnlock()
	m.publish("overall", data)
}

func (m *Manager) setLog(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	m.mu.Lock()
	m.lastLog = message
	m.touchLocked()
	m.mu.Unlock()
	m.publish("log", message)
}

func (m *Manager) touchLocked() { m.updatedAt = time.Now() }

func (m *Manager) publish(eventType string, data any) {
	event := Event{Type: eventType, Data: data}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for ch := range m.subscribers {
		select {
		case ch <- event:
		default:
			// 浏览器重新连接时会收到完整 snapshot；单个慢客户端不应阻塞监测。
		}
	}
}

func newSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
func cloneConnection(v *ConnectionInfo) *ConnectionInfo {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneSample(v *Sample) *Sample {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneSummary(v *Summary) *Summary {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }

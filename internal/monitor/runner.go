package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type streamEnvelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type connectedInfo struct {
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

type startData struct {
	Connected  []connectedInfo `json:"connected"`
	Version    string          `json:"version"`
	SystemInfo string          `json:"system_info"`
	PID        int             `json:"-"`
}

type intervalData struct {
	Streams         []intervalStream `json:"streams"`
	Sum             intervalStream   `json:"sum"`
	SumBidirReverse intervalStream   `json:"sum_bidir_reverse"`
}

type intervalStream struct {
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	Seconds       float64 `json:"seconds"`
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Packets       int64   `json:"packets"`
	Sender        bool    `json:"sender"`
	Omitted       bool    `json:"omitted"`
	JitterMs      float64 `json:"jitter_ms"`
	LostPackets   int64   `json:"lost_packets"`
	LossPercent   float64 `json:"lost_percent"`
	OutOfOrder    int64   `json:"out_of_order"`
	Retransmits   int64   `json:"retransmits"`
	SndCwnd       int64   `json:"snd_cwnd"`
	RTT           int64   `json:"rtt"`    // iperf3 以微秒返回。
	RTTVar        int64   `json:"rttvar"` // iperf3 以微秒返回。
	PMTU          int64   `json:"pmtu"`
}

type udpStats struct {
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	Seconds       float64 `json:"seconds"`
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	JitterMs      float64 `json:"jitter_ms"`
	LostPackets   int64   `json:"lost_packets"`
	Packets       int64   `json:"packets"`
	LossPercent   float64 `json:"lost_percent"`
	OutOfOrder    int64   `json:"out_of_order"`
	Sender        bool    `json:"sender"`
}

type endData struct {
	Streams     []endStream `json:"streams"`
	Sum         udpStats    `json:"sum"`
	SumReceived udpStats    `json:"sum_received"`
	CPU         struct {
		HostTotal    float64 `json:"host_total"`
		HostUser     float64 `json:"host_user"`
		HostSystem   float64 `json:"host_system"`
		RemoteTotal  float64 `json:"remote_total"`
		RemoteUser   float64 `json:"remote_user"`
		RemoteSystem float64 `json:"remote_system"`
	} `json:"cpu_utilization_percent"`
	Congestion string `json:"sender_tcp_congestion"`
}

type endStream struct {
	UDP      udpStats `json:"udp"`
	Sender   tcpStats `json:"sender"`
	Receiver tcpStats `json:"receiver"`
}

type tcpStats struct {
	Seconds       float64 `json:"seconds"`
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Retransmits   int64   `json:"retransmits"`
	MaxSndCwnd    int64   `json:"max_snd_cwnd"`
	MinRTT        int64   `json:"min_rtt"`
	MaxRTT        int64   `json:"max_rtt"`
	MeanRTT       int64   `json:"mean_rtt"`
	Sender        bool    `json:"sender"`
}

// serverIntervalRE 解析 --get-server-output 回传的服务端文本。
// iperf3 的 JSON 流在发送期间只包含客户端发送统计，真正的 UDP 丢包和
// jitter 由服务端在探测完成后回传，因此这里保留服务端逐区间结果。
var serverIntervalRE = regexp.MustCompile(`\[\s*\d+](?:\[[^]]+])?\s+([0-9.]+)-([0-9.]+)\s+sec\s+.*?\s+([0-9.]+)\s+(bits|Kbits|Mbits|Gbits)/sec\s+([0-9.]+)\s+ms\s+(\d+)/(\d+)\s+\(([0-9.]+)%\)`)

type serverLoss struct {
	Start, End, Bitrate, Jitter, Percent float64
	Lost, Total                          int64
}

func (m *Manager) runProbe(ctx context.Context, config Config, probe int) error {
	return m.runProtocolProbe(ctx, config, probe, config.Protocol)
}

func (m *Manager) runProtocolProbe(ctx context.Context, config Config, probe int, protocol string) error {
	args := buildArgs(config, protocol)
	cmd := exec.CommandContext(ctx, config.BinaryPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 iperf3 标准输出管道失败：%w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建 iperf3 错误输出管道失败：%w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 iperf3 失败（%s）：%w", config.BinaryPath, err)
	}
	m.mu.Lock()
	m.pid = cmd.Process.Pid
	m.touchLocked()
	m.mu.Unlock()
	m.publish("process", m.Snapshot())

	probeStarted := time.Now()
	var probeSamples []Sample
	var wg sync.WaitGroup
	var parseErr error
	var parseMu sync.Mutex

	// stdout 是 JSON 流，必须单独实时解析；不能等 cmd.Wait 后再读取。
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if err := m.handleJSONLine(line, probe, probeStarted, protocol, &probeSamples); err != nil {
				parseMu.Lock()
				parseErr = err
				parseMu.Unlock()
			}
		}
		if err := scanner.Err(); err != nil {
			parseMu.Lock()
			parseErr = fmt.Errorf("读取 iperf3 JSON 流失败：%w", err)
			parseMu.Unlock()
		}
	}()

	// stderr 主要用于传递连接失败、参数错误等文本，作为诊断信息展示。
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 8*1024), 256*1024)
		for scanner.Scan() {
			m.setLog(scanner.Text())
		}
	}()

	// StdoutPipe 和 StderrPipe 返回的管道会由 cmd.Wait 关闭。必须先等待两个
	// 扫描协程把管道读到 EOF，再调用 Wait 回收进程；如果顺序相反，Wait 可能
	// 在 Scanner 仍读取最后一批 JSON 时关闭文件，产生 "file already closed"，
	// 并把一次正常结束的探测错误地标记为失败。
	wg.Wait()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	parseMu.Lock()
	deferredParseErr := parseErr
	parseMu.Unlock()
	if deferredParseErr != nil {
		return deferredParseErr
	}
	if waitErr != nil {
		return fmt.Errorf("iperf3 %s 进程结束异常：%w", strings.ToUpper(protocol), waitErr)
	}
	return nil
}

func buildArgs(c Config, protocol string) []string {
	args := []string{"-c", c.Host, "-p", strconv.Itoa(c.Port),
		"-t", strconv.Itoa(c.ProbeDuration), "-i", formatSeconds(c.IntervalSeconds),
		"--json-stream", "--forceflush", "--get-server-output"}
	if protocol == "udp" {
		args = append(args, "-u", "-b", c.Bitrate, "--udp-counters-64bit")
	}
	if protocol == "udp" && c.PacketLength > 0 {
		args = append(args, "-l", strconv.Itoa(c.PacketLength))
	}
	if c.ParallelStreams > 1 {
		args = append(args, "-P", strconv.Itoa(c.ParallelStreams))
	}
	if c.Bidir {
		args = append(args, "--bidir")
	} else if c.Reverse {
		args = append(args, "-R")
	}
	if c.OmitSeconds > 0 {
		args = append(args, "-O", strconv.Itoa(c.OmitSeconds))
	}
	return args
}

func formatSeconds(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

func (m *Manager) handleJSONLine(line string, probe int, probeStarted time.Time, protocol string, samples *[]Sample) error {
	var envelope streamEnvelope
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		// --json-stream-full-output 未启用时这里理论上不会发生；忽略非 JSON
		// 文本能让不同版本 iperf3 的提示行不会中断整次监测。
		return nil
	}
	switch envelope.Event {
	case "start":
		var data startData
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("解析 iperf3 start 事件失败：%w", err)
		}
		m.setStart(data)
	case "interval":
		var data intervalData
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("解析 iperf3 interval 事件失败：%w", err)
		}
		streams := data.Streams
		if len(streams) == 0 {
			streams = []intervalStream{data.Sum}
			if data.SumBidirReverse.BitsPerSecond > 0 || data.SumBidirReverse.Packets > 0 {
				streams = append(streams, data.SumBidirReverse)
			}
		}
		for _, stream := range streams {
			if stream.Omitted {
				continue
			}
			direction := "upload"
			if !stream.Sender {
				direction = "download"
			}
			sample := Sample{
				ID: fmt.Sprintf("%s-%s-%d-%d", m.Snapshot().SessionID, protocol, probe, len(*samples)+1), Protocol: protocol, Probe: probe,
				Timestamp:    probeStarted.Add(time.Duration(stream.End * float64(time.Second))),
				StartSeconds: stream.Start, EndSeconds: stream.End, Duration: stream.Seconds,
				Bytes: stream.Bytes, BitsPerSecond: stream.BitsPerSecond, Packets: stream.Packets,
				Sender: stream.Sender, Source: "client", Direction: direction, Status: "live",
			}
			// 双向模式下，客户端接收方向本身就有 iperf3 的 UDP 序号统计，
			// 可以实时确认下载丢包；上传方向仍等待服务端回传。
			if protocol == "udp" && direction == "download" {
				sample.LostPackets, sample.TotalPackets = int64Ptr(stream.LostPackets), int64Ptr(stream.Packets)
				sample.LossPercent, sample.JitterMs = float64Ptr(stream.LossPercent), float64Ptr(stream.JitterMs)
				sample.OutOfOrder, sample.Status = int64Ptr(stream.OutOfOrder), "confirmed"
			} else if protocol == "tcp" {
				sample.Status = "confirmed"
				if stream.Sender {
					sample.Retransmits = int64Ptr(stream.Retransmits)
					sample.RTTMs = float64Ptr(float64(stream.RTT) / 1000)
					sample.RTTVarMs = float64Ptr(float64(stream.RTTVar) / 1000)
					sample.SndCwnd = int64Ptr(stream.SndCwnd)
					sample.PMTU = int64Ptr(stream.PMTU)
				}
			}
			*samples = append(*samples, sample)
			m.appendSample(sample)
		}
	case "server_output_text":
		if protocol != "udp" {
			return nil
		}
		var text string
		if err := json.Unmarshal(envelope.Data, &text); err != nil {
			return fmt.Errorf("解析服务端文本失败：%w", err)
		}
		m.applyServerOutput(text, probe, probeStarted, *samples)
	case "end":
		var data endData
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("解析 iperf3 end 事件失败：%w", err)
		}
		m.setEndSummaries(data, protocol)
	case "error":
		return fmt.Errorf("iperf3 返回错误：%s", readableJSON(envelope.Data))
	}
	return nil
}

func (m *Manager) applyServerOutput(text string, probe int, probeStarted time.Time, samples []Sample) {
	var results []serverLoss
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "receiver") {
			if result, ok := parseServerLine(line); ok {
				m.setDirectionSummary("udp", "upload", Summary{Duration: result.End, BitsPerSecond: result.Bitrate, LostPackets: result.Lost,
					Packets: result.Total, LossPercent: result.Percent, JitterMs: result.Jitter,
					CompletedAt: time.Now(), Confirmed: true})
			}
			continue
		}
		// 双向服务端汇总还会有 sender 行。它没有接收方向的丢包字段，
		// 也不能重复作为一个区间追加到明细表。
		if strings.Contains(line, "sender") {
			continue
		}
		if result, ok := parseServerLine(line); ok {
			results = append(results, result)
		}
	}
	var uploadSamples []Sample
	for _, sample := range samples {
		if sample.Direction == "upload" || sample.Direction == "" {
			uploadSamples = append(uploadSamples, sample)
		}
	}
	for i, result := range results {
		if i < len(uploadSamples) {
			m.updateSample(uploadSamples[i].ID, result.Lost, result.Total, result.Percent, result.Jitter, 0, "server")
			continue
		}
		// 服务端有时会比客户端多出最后一个收尾区间，不能丢弃这条丢包证据。
		sample := Sample{
			ID: fmt.Sprintf("%s-udp-%d-server-%d", m.Snapshot().SessionID, probe, i+1), Protocol: "udp", Probe: probe,
			Timestamp:    probeStarted.Add(time.Duration(result.End * float64(time.Second))),
			StartSeconds: result.Start, EndSeconds: result.End, Duration: result.End - result.Start,
			BitsPerSecond: result.Bitrate, LostPackets: int64Ptr(result.Lost), TotalPackets: int64Ptr(result.Total), Direction: "upload",
			LossPercent: float64Ptr(result.Percent), JitterMs: float64Ptr(result.Jitter), Source: "server", Status: "confirmed",
		}
		m.appendSample(sample)
	}
}

func parseServerLine(line string) (serverLoss, bool) {
	match := serverIntervalRE.FindStringSubmatch(line)
	if len(match) != 9 {
		return serverLoss{}, false
	}
	start, e1 := strconv.ParseFloat(match[1], 64)
	end, e2 := strconv.ParseFloat(match[2], 64)
	bitrate, e3 := strconv.ParseFloat(match[3], 64)
	jitter, e4 := strconv.ParseFloat(match[5], 64)
	lost, e5 := strconv.ParseInt(match[6], 10, 64)
	total, e6 := strconv.ParseInt(match[7], 10, 64)
	percent, e7 := strconv.ParseFloat(match[8], 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || e7 != nil {
		return serverLoss{}, false
	}
	// 使用明确分支，避免 map 迭代顺序影响单位换算。
	unit := match[4]
	switch unit {
	case "Kbits":
		bitrate *= 1e3
	case "Mbits":
		bitrate *= 1e6
	case "Gbits":
		bitrate *= 1e9
	}
	return serverLoss{Start: start, End: end, Bitrate: bitrate, Jitter: jitter, Lost: lost, Total: total, Percent: percent}, true
}

func (m *Manager) setEndSummaries(data endData, protocol string) {
	cpu := &CPUInfo{HostTotal: data.CPU.HostTotal, HostUser: data.CPU.HostUser, HostSystem: data.CPU.HostSystem,
		RemoteTotal: data.CPU.RemoteTotal, RemoteUser: data.CPU.RemoteUser, RemoteSystem: data.CPU.RemoteSystem}
	if protocol == "tcp" {
		for _, item := range data.Streams {
			direction := "upload"
			if !item.Sender.Sender {
				direction = "download"
			}
			summary := Summary{
				Duration: item.Sender.Seconds, Bytes: item.Sender.Bytes, BitsPerSecond: item.Sender.BitsPerSecond,
				ReceiverBitsPerSecond: item.Receiver.BitsPerSecond, Retransmits: item.Sender.Retransmits,
				MeanRTTMs: float64(item.Sender.MeanRTT) / 1000, MaxRTTMs: float64(item.Sender.MaxRTT) / 1000,
				// iperf3 的 TCP 结束统计提供 min_rtt/max_rtt，但不提供
				// 独立的 rttvar 字段，因此这里使用该周期内的 RTT 范围，
				// 作为“RTT 波动”的可解释、可跨平台展示值。
				RTTVarMs: rttRangeMs(item.Sender), SndCwnd: item.Sender.MaxSndCwnd,
				Congestion: data.Congestion, CPU: cpu, CompletedAt: time.Now(), Confirmed: true,
			}
			m.setDirectionSummary("tcp", direction, summary)
		}
		return
	}
	if len(data.Streams) > 0 {
		for _, item := range data.Streams {
			stream := item.UDP
			direction := "upload"
			if !stream.Sender {
				direction = "download"
			}
			m.setDirectionSummary("udp", direction, summaryFromUDP(stream, cpu))
		}
		return
	}
	// 兼容非双向模式或较老 iperf3 的 end 结构。
	upload := summaryFromUDP(data.Sum, cpu)
	upload.ReceiverBitsPerSecond = data.SumReceived.BitsPerSecond
	m.setDirectionSummary("udp", "upload", upload)
}

// rttRangeMs 将 iperf3 的 TCP 最小/最大 RTT（微秒）转换成毫秒范围。
// 某些 iperf3 或系统内核不会提供完整 RTT 字段，此时返回 0，前端会显示为缺失值。
func rttRangeMs(stats tcpStats) float64 {
	if stats.MinRTT <= 0 || stats.MaxRTT <= 0 || stats.MaxRTT < stats.MinRTT {
		return 0
	}
	return float64(stats.MaxRTT-stats.MinRTT) / 1000
}

func summaryFromUDP(stream udpStats, cpu *CPUInfo) Summary {
	return Summary{Duration: stream.Seconds, Bytes: stream.Bytes, BitsPerSecond: stream.BitsPerSecond,
		Packets: stream.Packets, LostPackets: stream.LostPackets, LossPercent: stream.LossPercent,
		JitterMs: stream.JitterMs, OutOfOrder: stream.OutOfOrder, CPU: cpu,
		CompletedAt: time.Now(), Confirmed: !stream.Sender}
}

func readableJSON(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

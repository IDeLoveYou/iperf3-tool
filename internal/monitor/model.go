package monitor

import "time"

// Config 是一次监测使用的全部 iperf3 参数。
// 字段直接对应网页配置项，避免把重要的网络行为隐藏在代码常量中。
type Config struct {
	Host            string  `json:"host"`
	Port            int     `json:"port"`
	Bitrate         string  `json:"bitrate"`
	IntervalSeconds float64 `json:"intervalSeconds"`
	ProbeDuration   int     `json:"probeDuration"`
	PacketLength    int     `json:"packetLength"`
	ParallelStreams int     `json:"parallelStreams"`
	BinaryPath      string  `json:"binaryPath"`
	Protocol        string  `json:"protocol"` // udp 或 tcp
	Continuous      bool    `json:"continuous"`
	Bidir           bool    `json:"bidir"`
	Reverse         bool    `json:"reverse"`
	OmitSeconds     int     `json:"omitSeconds"`
}

// Sample 是一个时间区间的监测结果。
// LossPercent 使用指针是为了区分“暂时还没有服务端回传丢包结果”和“丢包率为 0”。
type Sample struct {
	ID            string    `json:"id"`
	Protocol      string    `json:"protocol"` // udp 或 tcp
	Probe         int       `json:"probe"`
	Timestamp     time.Time `json:"timestamp"`
	StartSeconds  float64   `json:"startSeconds"`
	EndSeconds    float64   `json:"endSeconds"`
	Duration      float64   `json:"duration"`
	Bytes         int64     `json:"bytes"`
	BitsPerSecond float64   `json:"bitsPerSecond"`
	Packets       int64     `json:"packets"`
	LostPackets   *int64    `json:"lostPackets"`
	TotalPackets  *int64    `json:"totalPackets"`
	LossPercent   *float64  `json:"lossPercent"`
	JitterMs      *float64  `json:"jitterMs"`
	OutOfOrder    *int64    `json:"outOfOrder"`
	Sender        bool      `json:"sender"`
	Source        string    `json:"source"`    // client、server 或 server-summary
	Direction     string    `json:"direction"` // upload 或 download
	Status        string    `json:"status"`    // live、confirmed、error
	Retransmits   *int64    `json:"retransmits"`
	RTTMs         *float64  `json:"rttMs"`
	RTTVarMs      *float64  `json:"rttVarMs"`
	SndCwnd       *int64    `json:"sndCwnd"`
	PMTU          *int64    `json:"pmtu"`
}

// ConnectionInfo 保存 iperf3 建立连接时返回的本地/远端信息。
type ConnectionInfo struct {
	LocalHost  string `json:"localHost"`
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
}

type CPUInfo struct {
	HostTotal    float64 `json:"hostTotal"`
	HostUser     float64 `json:"hostUser"`
	HostSystem   float64 `json:"hostSystem"`
	RemoteTotal  float64 `json:"remoteTotal"`
	RemoteUser   float64 `json:"remoteUser"`
	RemoteSystem float64 `json:"remoteSystem"`
}

// Summary 是当前探测周期的聚合结果。
type Summary struct {
	Duration              float64   `json:"duration"`
	Bytes                 int64     `json:"bytes"`
	BitsPerSecond         float64   `json:"bitsPerSecond"`
	Packets               int64     `json:"packets"`
	LostPackets           int64     `json:"lostPackets"`
	LossPercent           float64   `json:"lossPercent"`
	JitterMs              float64   `json:"jitterMs"`
	OutOfOrder            int64     `json:"outOfOrder"`
	ReceiverBitsPerSecond float64   `json:"receiverBitsPerSecond"`
	CPU                   *CPUInfo  `json:"cpu,omitempty"`
	CompletedAt           time.Time `json:"completedAt"`
	Confirmed             bool      `json:"confirmed"`
	Protocol              string    `json:"protocol"`
	Retransmits           int64     `json:"retransmits"`
	MeanRTTMs             float64   `json:"meanRttMs"`
	MaxRTTMs              float64   `json:"maxRttMs"`
	RTTVarMs              float64   `json:"rttVarMs"`
	SndCwnd               int64     `json:"sndCwnd"`
	Congestion            string    `json:"congestion,omitempty"`
}

// Snapshot 是网页一次刷新所需的完整状态，SSE 的 snapshot 事件也使用它。
type Snapshot struct {
	State           string          `json:"state"` // idle、running、stopping、error
	SessionID       string          `json:"sessionId"`
	StartedAt       *time.Time      `json:"startedAt,omitempty"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	Probe           int             `json:"probe"`
	PID             int             `json:"pid,omitempty"`
	Config          Config          `json:"config"`
	Connection      *ConnectionInfo `json:"connection,omitempty"`
	IperfVersion    string          `json:"iperfVersion,omitempty"`
	SystemInfo      string          `json:"systemInfo,omitempty"`
	Latest          *Sample         `json:"latest,omitempty"`
	UploadLatest    *Sample         `json:"uploadLatest,omitempty"`
	DownloadLatest  *Sample         `json:"downloadLatest,omitempty"`
	Summary         *Summary        `json:"summary,omitempty"`
	UploadSummary   *Summary        `json:"uploadSummary,omitempty"`
	DownloadSummary *Summary        `json:"downloadSummary,omitempty"`
	OverallUpload   *Summary        `json:"overallUpload,omitempty"`
	OverallDownload *Summary        `json:"overallDownload,omitempty"`
	History         []Sample        `json:"history"`
	LastError       string          `json:"lastError,omitempty"`
	LastLog         string          `json:"lastLog,omitempty"`
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func DefaultConfig() Config {
	return Config{
		Host:            "127.0.0.1",
		Port:            5201,
		Bitrate:         "1M",
		IntervalSeconds: 1,
		ProbeDuration:   10,
		PacketLength:    256,
		ParallelStreams: 1,
		BinaryPath:      "iperf3",
		Protocol:        "udp",
		Continuous:      true,
		Bidir:           true,
		OmitSeconds:     3,
	}
}

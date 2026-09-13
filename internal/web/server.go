package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"iperf3-tool/internal/monitor"
)

//go:embed index.html favicon.svg assets/*
var page embed.FS

type Server struct {
	hub *monitor.Hub
}

func NewServer(hub *monitor.Hub) *Server { return &Server{hub: hub} }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		s.servePage(w)
	case r.URL.Path == "/favicon.svg" && r.Method == http.MethodGet:
		content, err := page.ReadFile("favicon.svg")
		if err != nil {
			http.Error(w, "图标资源加载失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(content)
	case strings.HasPrefix(r.URL.Path, "/assets/") && r.Method == http.MethodGet:
		// Element Plus 选择器已在构建阶段打包到 assets，运行时不依赖 CDN。
		http.FileServer(http.FS(page)).ServeHTTP(w, r)
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.URL.Path == "/api/tests" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.hub.List())
	case r.URL.Path == "/api/tests/start" && r.Method == http.MethodPost:
		var config monitor.Config
		if err := decodeJSON(w, r, &config); err != nil {
			return
		}
		result, err := s.hub.CreateAndStart(r.Context(), config)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, result)
	case r.URL.Path == "/api/tests" && r.Method == http.MethodDelete:
		result, err := s.hub.Delete(r.URL.Query().Get("testId"))
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.URL.Path == "/api/status" && r.Method == http.MethodGet:
		manager, ok := s.managerFor(w, r)
		if ok {
			writeJSON(w, http.StatusOK, manager.Snapshot())
		}
	case r.URL.Path == "/api/config" && r.Method == http.MethodGet:
		manager, ok := s.managerFor(w, r)
		if ok {
			writeJSON(w, http.StatusOK, manager.Config())
		}
	case r.URL.Path == "/api/start" && r.Method == http.MethodPost:
		s.start(w, r)
	case r.URL.Path == "/api/stop" && r.Method == http.MethodPost:
		manager, ok := s.managerFor(w, r)
		if !ok {
			return
		}
		if err := manager.Stop(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, manager.Snapshot())
	case r.URL.Path == "/api/stream" && r.Method == http.MethodGet:
		s.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) servePage(w http.ResponseWriter) {
	content, err := page.ReadFile("index.html")
	if err != nil {
		http.Error(w, "页面资源加载失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var config monitor.Config
	if err := decodeJSON(w, r, &config); err != nil {
		return
	}
	// 新配置先在标签之外启动。只有收到首条有效数据后，Hub 才会
	// 原子替换当前标签；因此校验、重复配置或 iperf3 启动失败都不会污染旧配置。
	result, err := s.hub.ReplaceAndStart(r.Context(), r.URL.Query().Get("testId"), config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	manager, found := s.managerFor(w, r)
	if !found {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前 HTTP 服务不支持实时推送", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	channel, closeChannel := manager.Subscribe()
	defer closeChannel()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-channel:
			if !open {
				return
			}
			payload, err := json.Marshal(event.Data)
			if err != nil {
				continue
			}
			// SSE 的 event 名只来自服务端固定枚举，不直接使用用户输入。
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) managerFor(w http.ResponseWriter, r *http.Request) (*monitor.Manager, bool) {
	manager, err := s.hub.Get(r.URL.Query().Get("testId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return nil, false
	}
	return manager, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(r.Body)
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求 JSON 无效：%w", err))
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "请求失败"
	}
	writeJSON(w, status, map[string]string{"error": message})
}

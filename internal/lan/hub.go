package lan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const nodeTTL = 35 * time.Second

type Hub struct {
	token     string
	advertise string
	mu        sync.RWMutex
	nodes     map[string]NodeInfo
	client    *http.Client
	next      uint64
	gateway   GatewayConfig
}

func NewHub(token, advertise string) (*Hub, error) {
	if advertise != "" && !validHTTPURL(advertise) {
		return nil, errors.New("ADVERTISE_URL must be an HTTP(S) origin")
	}
	return &Hub{token: token, advertise: strings.TrimRight(advertise, "/"), nodes: make(map[string]NodeInfo), client: newHTTPClient()}, nil
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/nodes", h.register)
	mux.HandleFunc("GET /v1/nodes", h.list)
	mux.HandleFunc("GET /v1/traefik", h.traefik)
	mux.HandleFunc("GET /v1/gateway/status", h.gatewayStatus)
	mux.HandleFunc("/v1/files/", h.files)
	requireAuth(h.token, mux).ServeHTTP(w, r)
}

func (h *Hub) register(w http.ResponseWriter, r *http.Request) {
	var node NodeInfo
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&node); err != nil || node.ID == "" || len(node.ID) > 100 || strings.ContainsAny(node.ID, "/\\ \t\r\n") || !validHTTPURL(node.FileURL) || (node.MinioURL != "" && !validHTTPURL(node.MinioURL)) || (node.MinioConsoleURL != "" && !validHTTPURL(node.MinioConsoleURL)) || (node.PostgresAddress != "" && !validPostgresAddress(node.PostgresAddress)) {
		writeError(w, 400, "invalid node registration")
		return
	}
	if node.PostgresRole != "primary" && node.PostgresRole != "standby" {
		node.PostgresRole = "unknown"
	}
	node.LastSeen = time.Now().UTC()
	h.mu.Lock()
	h.nodes[node.ID] = node
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"expires_in_seconds": int(nodeTTL.Seconds())})
}

func (h *Hub) activeNodes() []NodeInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]NodeInfo, 0, len(h.nodes))
	for _, node := range h.nodes {
		if time.Since(node.LastSeen) < nodeTTL {
			result = append(result, node)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (h *Hub) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"nodes": h.activeNodes()})
}

func (h *Hub) files(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	if !validKey(key) {
		writeError(w, 400, "invalid file key")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeError(w, 405, "method not allowed")
		return
	}
	nodes := h.activeNodes()
	if len(nodes) == 0 {
		writeError(w, 503, "no active storage nodes")
		return
	}
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		h.mu.Lock()
		index := int(h.next % uint64(len(nodes)))
		h.next++
		h.mu.Unlock()
		nodes = nodes[index : index+1]
	}
	notFound := 0
	for _, node := range nodes {
		base, _ := url.Parse(node.FileURL)
		base.Path = "/v1/files/" + key
		base.RawPath = ""
		base.RawQuery = r.URL.RawQuery
		request, err := http.NewRequestWithContext(r.Context(), r.Method, base.String(), r.Body)
		if err != nil {
			break
		}
		request.Header = r.Header.Clone()
		response, err := h.client.Do(request)
		if err != nil {
			log.Printf("node %s request failed: %v", node.ID, err)
			continue
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && response.StatusCode == http.StatusNotFound {
			response.Body.Close()
			notFound++
			continue
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && response.StatusCode >= 500 {
			response.Body.Close()
			continue
		}
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		response.Body.Close()
		return
	}
	if notFound == len(nodes) {
		writeError(w, 404, "file not found")
		return
	}
	writeError(w, 503, "file unavailable on active nodes")
}

func (h *Hub) RunDiscovery(ctx context.Context, address string) {
	if h.advertise == "" {
		return
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		log.Printf("discovery address: %v", err)
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		log.Printf("discovery port: %v", err)
		return
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(239, 255, 41, 43), Port: port})
	if err != nil {
		log.Printf("discovery listener: %v", err)
		return
	}
	defer conn.Close()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	buffer := make([]byte, 256)
	for {
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		if string(buffer[:n]) == "KRATOS_LAN_DISCOVER_V1" {
			_, _ = conn.WriteTo([]byte("KRATOS_LAN_HUB_V1 "+h.advertise), peer)
		}
	}
}

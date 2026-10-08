package lan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NodeConfig struct {
	ID, Token, DataDir, FileURL, MinioURL, MinioConsoleURL, PostgresAddress, HubURL, DiscoveryPort string
	PostgresProbe                                                                                  PostgresProbe
	ReplicaCount                                                                                   int
	MaxFileBytes                                                                                   int64
}

type Node struct {
	config    NodeConfig
	client    *http.Client
	hubURL    string
	hubMu     sync.RWMutex
	fileMu    sync.Mutex
	replicaMu sync.Mutex
}

func NewNode(c NodeConfig) (*Node, error) {
	if c.ID == "" || len(c.ID) > 100 || strings.ContainsAny(c.ID, "/\\ \t\r\n") || !validHTTPURL(c.FileURL) || (c.HubURL != "" && !validHTTPURL(c.HubURL)) || (c.MinioURL != "" && !validHTTPURL(c.MinioURL)) {
		return nil, errors.New("NODE_ID, FILE_URL and optional HUB_URL/MINIO_URL must be valid")
	}
	if c.ReplicaCount < 1 || c.MaxFileBytes < 1 {
		return nil, errors.New("invalid replica count or file size limit")
	}
	if c.MinioConsoleURL != "" && !validHTTPURL(c.MinioConsoleURL) {
		return nil, errors.New("invalid MINIO_CONSOLE_URL")
	}
	if c.PostgresAddress != "" && !validPostgresAddress(c.PostgresAddress) {
		return nil, errors.New("POSTGRES_ADDRESS must be host:port")
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return nil, err
	}
	return &Node{config: c, client: newHTTPClient(), hubURL: strings.TrimRight(c.HubURL, "/")}, nil
}

func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/sync", n.syncFiles)
	mux.HandleFunc("/v1/files/", n.files)
	mux.HandleFunc("/internal/files/", n.files)
	requireAuth(n.config.Token, mux).ServeHTTP(w, r)
}

func (n *Node) files(w http.ResponseWriter, r *http.Request) {
	replica := strings.HasPrefix(r.URL.Path, "/internal/files/")
	prefix := "/v1/files/"
	if replica {
		prefix = "/internal/files/"
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)
	if !validKey(key) || r.URL.RawPath != "" && !validKey(strings.TrimPrefix(r.URL.EscapedPath(), prefix)) {
		writeError(w, 400, "invalid file key")
		return
	}
	path := filepath.Join(n.config.DataDir, filepath.FromSlash(key))
	// Replica requests never call another node; a separate lock avoids circular
	// waits when client uploads are concurrently handled by two different nodes.
	writeMu := &n.fileMu
	if replica {
		writeMu = &n.replicaMu
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			writeError(w, 404, "file not found")
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=60")
		http.ServeFile(w, r, path)
	case http.MethodPut:
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := n.put(w, r, path, key, replica); err != nil {
			writeError(w, 503, err.Error())
		}
	case http.MethodDelete:
		writeMu.Lock()
		defer writeMu.Unlock()
		if !replica {
			if err := n.replicateDelete(r.Context(), key); err != nil {
				writeError(w, 503, err.Error())
				return
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			writeError(w, 500, "delete failed")
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (n *Node) put(w http.ResponseWriter, r *http.Request, path, key string, replica bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	count, err := io.Copy(f, io.LimitReader(r.Body, n.config.MaxFileBytes+1))
	if err != nil {
		return err
	}
	if count > n.config.MaxFileBytes {
		writeError(w, 413, "file too large")
		return nil
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if !replica {
		if err := n.replicatePut(r.Context(), f.Name(), key); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (n *Node) peers(ctx context.Context) ([]NodeInfo, error) {
	if n.config.ReplicaCount == 1 {
		return nil, nil
	}
	n.hubMu.RLock()
	hub := n.hubURL
	n.hubMu.RUnlock()
	if hub == "" {
		return nil, errors.New("hub unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", hub+"/v1/nodes", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+n.config.Token)
	response, err := n.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("hub returned %d", response.StatusCode)
	}
	var body struct {
		Nodes []NodeInfo `json:"nodes"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	result := make([]NodeInfo, 0, len(body.Nodes))
	for _, peer := range body.Nodes {
		if peer.ID != n.config.ID {
			result = append(result, peer)
		}
	}
	return result, nil
}

func (n *Node) replicatePut(ctx context.Context, path, key string) error {
	if n.config.ReplicaCount == 1 {
		return nil
	}
	peers, err := n.peers(ctx)
	if err != nil {
		return err
	}
	if len(peers)+1 < n.config.ReplicaCount {
		return fmt.Errorf("need %d nodes, found %d", n.config.ReplicaCount, len(peers)+1)
	}
	completed := 1
	for _, peer := range peers {
		if err := n.putPeer(ctx, peer, path, key); err == nil {
			completed++
		}
		if completed >= n.config.ReplicaCount {
			return nil
		}
	}
	return fmt.Errorf("only %d of %d required file copies completed", completed, n.config.ReplicaCount)
}

func (n *Node) putPeer(ctx context.Context, peer NodeInfo, path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	endpoint := strings.TrimRight(peer.FileURL, "/") + "/internal/files/" + escapeKey(key)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, f)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+n.config.Token)
	response, err := n.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("peer %s returned %d", peer.ID, response.StatusCode)
	}
	return nil
}

func (n *Node) replicateDelete(ctx context.Context, key string) error {
	peers, err := n.peers(ctx)
	if err != nil {
		return err
	}
	if len(peers)+1 < n.config.ReplicaCount {
		return fmt.Errorf("need %d nodes, found %d", n.config.ReplicaCount, len(peers)+1)
	}
	for _, peer := range peers {
		endpoint := strings.TrimRight(peer.FileURL, "/") + "/internal/files/" + escapeKey(key)
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+n.config.Token)
		response, err := n.client.Do(request)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode != 204 {
			return fmt.Errorf("peer %s returned %d", peer.ID, response.StatusCode)
		}
	}
	return nil
}

func (n *Node) syncFiles(w http.ResponseWriter, r *http.Request) {
	if n.config.ReplicaCount == 1 {
		writeError(w, 409, "set REPLICA_COUNT above 1 first")
		return
	}
	peers, err := n.peers(r.Context())
	if err != nil || len(peers)+1 < n.config.ReplicaCount {
		writeError(w, 503, "required peers are unavailable")
		return
	}
	count := 0
	err = filepath.WalkDir(n.config.DataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".upload-") {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(n.config.DataDir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !validKey(key) {
			return fmt.Errorf("invalid stored path %q", key)
		}
		for _, peer := range peers {
			endpoint := strings.TrimRight(peer.FileURL, "/") + "/internal/files/" + escapeKey(key)
			request, err := http.NewRequestWithContext(r.Context(), http.MethodHead, endpoint, nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", "Bearer "+n.config.Token)
			response, err := n.client.Do(request)
			if err != nil {
				return err
			}
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				continue
			}
			if response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("peer %s returned %d", peer.ID, response.StatusCode)
			}
			if err := n.putPeer(r.Context(), peer, path, key); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"files_synced": count})
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (n *Node) RunRegistration(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := n.register(ctx); err != nil {
			log.Printf("node registration: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Node) register(ctx context.Context) error {
	n.hubMu.RLock()
	hub := n.hubURL
	n.hubMu.RUnlock()
	if hub == "" {
		var err error
		hub, err = discover(ctx, n.config.DiscoveryPort)
		if err != nil {
			return err
		}
		n.hubMu.Lock()
		n.hubURL = hub
		n.hubMu.Unlock()
	}
	info := NodeInfo{ID: n.config.ID, FileURL: n.config.FileURL, MinioURL: n.config.MinioURL, MinioConsoleURL: n.config.MinioConsoleURL, PostgresAddress: n.config.PostgresAddress, PostgresRole: "unknown"}
	if info.MinioURL != "" {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, strings.TrimRight(info.MinioURL, "/")+"/minio/health/live", nil)
		if err == nil {
			response, err := n.client.Do(request)
			if err == nil {
				info.MinioHealthy = response.StatusCode == 200
				response.Body.Close()
			}
		}
		cancel()
	}
	if info.PostgresAddress != "" {
		info.PostgresHealthy, info.PostgresRole = probePostgres(ctx, info.PostgresAddress, n.config.PostgresProbe)
	}
	data, _ := json.Marshal(info)
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, hub+"/v1/nodes", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+n.config.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := n.client.Do(request)
	if err != nil {
		n.hubMu.Lock()
		if n.config.HubURL == "" {
			n.hubURL = ""
		}
		n.hubMu.Unlock()
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("hub returned %d", response.StatusCode)
	}
	return nil
}

func discover(ctx context.Context, port string) (string, error) {
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return "", errors.New("invalid discovery port")
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if udp, ok := conn.(*net.UDPConn); ok {
		_ = udp.SetWriteBuffer(1024)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.WriteTo([]byte("KRATOS_LAN_DISCOVER_V1"), &net.UDPAddr{IP: net.IPv4(239, 255, 41, 43), Port: portNum})
	if err != nil {
		return "", err
	}
	buffer := make([]byte, 512)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		count, _, err := conn.ReadFrom(buffer)
		if err != nil {
			return "", err
		}
		value := strings.TrimPrefix(string(buffer[:count]), "KRATOS_LAN_HUB_V1 ")
		if value != string(buffer[:count]) && validHTTPURL(value) {
			return strings.TrimRight(value, "/"), nil
		}
	}
}

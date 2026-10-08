package lan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTwoNodeFileReplicationAndReadFallback(t *testing.T) {
	const token = "0123456789abcdefghijklmnopqrstuv"
	hub, err := NewHub(token, "")
	if err != nil {
		t.Fatal(err)
	}
	hubServer := httptest.NewServer(hub)
	defer hubServer.Close()
	var nodes []*Node
	var servers []*httptest.Server
	for _, id := range []string{"a", "b"} {
		node, err := NewNode(NodeConfig{ID: id, Token: token, DataDir: t.TempDir(), FileURL: "http://127.0.0.1", HubURL: hubServer.URL, ReplicaCount: 2, MaxFileBytes: 1024, DiscoveryPort: "39471"})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(node)
		defer server.Close()
		node.config.FileURL = server.URL
		nodes = append(nodes, node)
		servers = append(servers, server)
		data, _ := json.Marshal(NodeInfo{ID: id, FileURL: server.URL})
		response := call(t, http.MethodPost, hubServer.URL+"/v1/nodes", token, data)
		if response.StatusCode != 200 {
			t.Fatalf("register %s: %d", id, response.StatusCode)
		}
		response.Body.Close()
	}
	response := call(t, http.MethodPut, hubServer.URL+"/v1/files/docs/test.txt", token, []byte("hello replica"))
	if response.StatusCode != 201 {
		t.Fatalf("put: %d", response.StatusCode)
	}
	response.Body.Close()
	for _, node := range nodes {
		data, err := os.ReadFile(filepath.Join(node.config.DataDir, "docs", "test.txt"))
		if err != nil || string(data) != "hello replica" {
			t.Fatalf("node %s copy: %q %v", node.config.ID, data, err)
		}
	}
	if err := os.Remove(filepath.Join(nodes[0].config.DataDir, "docs", "test.txt")); err != nil {
		t.Fatal(err)
	}
	response = call(t, http.MethodGet, hubServer.URL+"/v1/files/docs/test.txt", token, nil)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(data) != "hello replica" {
		t.Fatalf("fallback: %d %q", response.StatusCode, data)
	}
	response = call(t, http.MethodDelete, hubServer.URL+"/v1/files/docs/test.txt", token, nil)
	if response.StatusCode != 204 {
		t.Fatalf("delete: %d", response.StatusCode)
	}
	response.Body.Close()
	for _, node := range nodes {
		_, err := os.Stat(filepath.Join(node.config.DataDir, "docs", "test.txt"))
		if !os.IsNotExist(err) {
			t.Fatalf("node %s still contains file: %v", node.config.ID, err)
		}
	}
	if err := os.WriteFile(filepath.Join(nodes[0].config.DataDir, "old.txt"), []byte("preexisting"), 0600); err != nil {
		t.Fatal(err)
	}
	response = call(t, http.MethodPost, servers[0].URL+"/v1/sync", token, nil)
	if response.StatusCode != 200 {
		t.Fatalf("sync: %d", response.StatusCode)
	}
	response.Body.Close()
	copied, err := os.ReadFile(filepath.Join(nodes[1].config.DataDir, "old.txt"))
	if err != nil || string(copied) != "preexisting" {
		t.Fatalf("old file not copied: %q %v", copied, err)
	}
	// Make both nodes start their outgoing replica request while each still has
	// an active client upload; a shared client/replica lock would deadlock here.
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/internal/files/") {
			arrived <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	for _, node := range nodes {
		node.client = &http.Client{Transport: transport, Timeout: 4 * time.Second}
	}
	completed := make(chan error, 2)
	for i, server := range servers {
		go func(index int, endpoint string) {
			request, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/v1/files/concurrent-%d.txt", endpoint, index), strings.NewReader("parallel"))
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != 201 {
					err = fmt.Errorf("concurrent write status %d", response.StatusCode)
				}
			}
			completed <- err
		}(i, server.URL)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("uploads did not reach replica stage")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRejectsInvalidKeysAndUnauthenticatedAccess(t *testing.T) {
	for _, key := range []string{"", "../x", "a/../b", "a//b", "a\\b", "/abs"} {
		if validKey(key) {
			t.Errorf("accepted %q", key)
		}
	}
	hub, _ := NewHub("0123456789abcdefghijklmnopqrstuv", "")
	server := httptest.NewServer(hub)
	defer server.Close()
	response := call(t, http.MethodGet, server.URL+"/v1/nodes", "", nil)
	if response.StatusCode != 401 {
		t.Fatalf("unauthorized: %d", response.StatusCode)
	}
	response.Body.Close()
}

func TestSingleNodeWriteBeforeSecondNodeExists(t *testing.T) {
	const token = "0123456789abcdefghijklmnopqrstuv"
	node, err := NewNode(NodeConfig{ID: "first", Token: token, DataDir: t.TempDir(), FileURL: "http://127.0.0.1", ReplicaCount: 1, MaxFileBytes: 1024, DiscoveryPort: "39471"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(node)
	defer server.Close()
	response := call(t, http.MethodPut, server.URL+"/v1/files/one.txt", token, []byte("ready"))
	if response.StatusCode != 201 {
		t.Fatalf("single node put: %d", response.StatusCode)
	}
	response.Body.Close()
}

func call(t *testing.T, method, endpoint, token string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

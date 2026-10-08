package lan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestGatewayRoutesOnlyConfirmedPrimaryAndSelectedMinioSite(t *testing.T) {
	hub, _ := NewHub("0123456789abcdefghijklmnopqrstuv", "")
	if err := hub.ConfigureGateway(GatewayConfig{HubURL: "http://hub:8080", Domain: "kratos.internal", MinioNodeID: "a"}); err != nil {
		t.Fatal(err)
	}
	hub.nodes["a"] = NodeInfo{ID: "a", FileURL: "http://a:8080", MinioURL: "http://a:9000", MinioConsoleURL: "http://a:9001", MinioHealthy: true, PostgresAddress: "a:5432", PostgresHealthy: true, PostgresRole: "primary", LastSeen: time.Now()}
	hub.nodes["b"] = NodeInfo{ID: "b", FileURL: "http://b:8080", MinioURL: "http://b:9000", MinioHealthy: true, PostgresAddress: "b:5432", PostgresHealthy: true, PostgresRole: "standby", LastSeen: time.Now()}
	config, status := hub.gatewaySnapshot()
	if status.PostgresNode != "a" || status.MinioNode != "a" || !status.Files {
		t.Fatalf("unexpected status: %+v", status)
	}
	if got := config.TCP.Services["kratos-postgres"].LoadBalancer.Servers[0].Address; got != "a:5432" {
		t.Fatalf("write routed to %s", got)
	}
	if got := config.HTTP.Services["kratos-s3"].LoadBalancer.Servers; len(got) != 1 || got[0].URL != "http://a:9000" {
		t.Fatalf("S3 site selection: %+v", got)
	}
	if got := config.HTTP.Services["kratos-files"].LoadBalancer.Servers[0].URL; got != "http://hub:8080" {
		t.Fatalf("file requests bypass Hub: %s", got)
	}

	second := hub.nodes["b"]
	second.PostgresRole = "primary"
	hub.nodes["b"] = second
	config, status = hub.gatewaySnapshot()
	if len(config.TCP.Routers) != 0 || status.PostgresState != "multiple_primaries_blocked" {
		t.Fatal("multiple primaries must disable write route")
	}

	first := hub.nodes["a"]
	first.LastSeen = time.Now().Add(-nodeTTL - time.Second)
	hub.nodes["a"] = first
	second.PostgresRole = "standby"
	hub.nodes["b"] = second
	config, status = hub.gatewaySnapshot()
	if _, exists := config.HTTP.Routers["kratos-s3"]; exists {
		t.Fatal("S3 must not silently switch to an async replica")
	}
	if len(config.TCP.Routers) != 0 || status.PostgresState != "no_confirmed_primary" {
		t.Fatal("standby must not become a write backend")
	}
	second.LastSeen = time.Now().Add(-nodeTTL - time.Second)
	hub.nodes["b"] = second
	config, _ = hub.gatewaySnapshot()
	if _, exists := config.HTTP.Routers["kratos-files"]; exists {
		t.Fatal("expired nodes must remove file route")
	}
	if _, exists := config.HTTP.Routers["kratos-registry"]; !exists {
		t.Fatal("registry must stay accessible for returning nodes")
	}
}

func TestGatewayProviderAuthenticationAndUnknownRole(t *testing.T) {
	const token = "0123456789abcdefghijklmnopqrstuv"
	hub, _ := NewHub(token, "")
	if err := hub.ConfigureGateway(GatewayConfig{HubURL: "http://hub:8080"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	response := call(t, "GET", server.URL+"/v1/traefik", "", nil)
	if response.StatusCode != 401 {
		t.Fatal("provider endpoint must require auth")
	}
	response.Body.Close()
	data, _ := json.Marshal(NodeInfo{ID: "unknown", FileURL: "http://node:8080", PostgresAddress: "node:5432", PostgresHealthy: true})
	response = call(t, "POST", server.URL+"/v1/nodes", token, data)
	if response.StatusCode != 200 {
		t.Fatalf("register: %d", response.StatusCode)
	}
	response.Body.Close()
	response = call(t, "GET", server.URL+"/v1/traefik", token, nil)
	defer response.Body.Close()
	var config dynamicConfig
	if err := json.NewDecoder(response.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(config.TCP.Routers) != 0 {
		t.Fatal("an unknown PostgreSQL role must not get a write route")
	}
	if err := hub.ConfigureGateway(GatewayConfig{HubURL: "http://hub:8080", Domain: "bad`) || Host(`other"}); err == nil {
		t.Fatal("accepted unsafe domain")
	}
}

func TestPostgresRolesIntegration(t *testing.T) {
	primary, standby := os.Getenv("KRATOS_TEST_PRIMARY"), os.Getenv("KRATOS_TEST_STANDBY")
	if primary == "" || standby == "" {
		t.Skip("requires temporary PostgreSQL primary and standby")
	}
	p := PostgresProbe{User: os.Getenv("KRATOS_TEST_PG_USER"), Password: os.Getenv("KRATOS_TEST_PG_PASSWORD"), Database: "app", SSLMode: "disable"}
	for address, want := range map[string]string{primary: "primary", standby: "standby"} {
		healthy, role := probePostgres(context.Background(), address, p)
		if !healthy || role != want {
			t.Fatalf("address %s: healthy=%t role=%s want=%s", address, healthy, role, want)
		}
	}
	p.Password = "wrong-password"
	if healthy, role := probePostgres(context.Background(), primary, p); healthy || role != "unknown" {
		t.Fatal("failed authentication must not report primary")
	}
}

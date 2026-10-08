package lan

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// GatewayConfig is fixed at startup. Node membership is read on each provider poll.
type GatewayConfig struct {
	HubURL             string
	Domain             string
	HTTPEntryPoint     string
	PostgresEntryPoint string
	MinioNodeID        string
	TLS                bool
	CertResolver       string
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func (h *Hub) ConfigureGateway(c GatewayConfig) error {
	if !validHTTPURL(c.HubURL) {
		return errors.New("GATEWAY_HUB_URL must be an HTTP(S) origin reachable from Traefik")
	}
	c.HubURL = strings.TrimRight(c.HubURL, "/")
	c.Domain = strings.ToLower(c.Domain)
	if c.Domain == "" {
		c.Domain = "kratos.internal"
	}
	if len(c.Domain) > 240 {
		return errors.New("GATEWAY_DOMAIN is too long")
	}
	for _, label := range strings.Split(c.Domain, ".") {
		if !dnsLabel.MatchString(label) {
			return errors.New("invalid GATEWAY_DOMAIN")
		}
	}
	if c.HTTPEntryPoint == "" {
		c.HTTPEntryPoint = "web"
	}
	if c.PostgresEntryPoint == "" {
		c.PostgresEntryPoint = "postgres"
	}
	if c.MinioNodeID == "" {
		c.MinioNodeID = "storage-1"
	}
	if c.CertResolver != "" && !c.TLS {
		return errors.New("GATEWAY_CERT_RESOLVER requires GATEWAY_TLS=true")
	}
	h.gateway = c
	return nil
}

type dynamicConfig struct {
	HTTP httpConfig `json:"http"`
	TCP  tcpConfig  `json:"tcp"`
}

// Traefik's configuration decoder rejects a standalone empty TCP routers map.
// Omit the entire section when no confirmed primary is available. Each HTTP
// provider response replaces the prior snapshot, so this also removes old routes.
func (c dynamicConfig) MarshalJSON() ([]byte, error) {
	type payload struct {
		HTTP httpConfig `json:"http"`
		TCP  *tcpConfig `json:"tcp,omitempty"`
	}
	p := payload{HTTP: c.HTTP}
	if len(c.TCP.Routers) > 0 {
		p.TCP = &c.TCP
	}
	return json.Marshal(p)
}

type httpConfig struct {
	Routers  map[string]httpRouter  `json:"routers"`
	Services map[string]httpService `json:"services"`
}

type httpRouter struct {
	EntryPoints []string       `json:"entryPoints"`
	Rule        string         `json:"rule"`
	Service     string         `json:"service"`
	TLS         *httpRouterTLS `json:"tls,omitempty"`
}

type httpRouterTLS struct {
	CertResolver string `json:"certResolver,omitempty"`
}

type httpService struct {
	LoadBalancer httpLoadBalancer `json:"loadBalancer"`
}

type httpLoadBalancer struct {
	Servers        []httpBackend `json:"servers"`
	PassHostHeader bool          `json:"passHostHeader"`
	HealthCheck    httpCheck     `json:"healthCheck"`
}

type httpBackend struct {
	URL string `json:"url"`
}
type httpCheck struct {
	Path     string `json:"path"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
}

type tcpConfig struct {
	Routers  map[string]tcpRouter  `json:"routers"`
	Services map[string]tcpService `json:"services"`
}

type tcpRouter struct {
	EntryPoints []string `json:"entryPoints"`
	Rule        string   `json:"rule"`
	Service     string   `json:"service"`
}

type tcpService struct {
	LoadBalancer tcpLoadBalancer `json:"loadBalancer"`
}
type tcpLoadBalancer struct {
	Servers []tcpBackend `json:"servers"`
}
type tcpBackend struct {
	Address string `json:"address"`
}

type gatewayStatus struct {
	ActiveNodes   int    `json:"active_nodes"`
	Files         bool   `json:"files_available"`
	MinioNode     string `json:"minio_node,omitempty"`
	MinioState    string `json:"minio_state"`
	PostgresNode  string `json:"postgres_node,omitempty"`
	PostgresState string `json:"postgres_state"`
}

func (h *Hub) gatewaySnapshot() (dynamicConfig, gatewayStatus) {
	config := dynamicConfig{
		HTTP: httpConfig{Routers: map[string]httpRouter{}, Services: map[string]httpService{}},
		TCP:  tcpConfig{Routers: map[string]tcpRouter{}, Services: map[string]tcpService{}},
	}
	nodes := h.activeNodes()
	status := gatewayStatus{ActiveNodes: len(nodes), Files: len(nodes) > 0, MinioState: "preferred_site_unavailable", PostgresState: "no_confirmed_primary"}
	c := h.gateway
	addHTTP := func(name, host, target, healthPath string) {
		router := httpRouter{EntryPoints: []string{c.HTTPEntryPoint}, Rule: "Host(`" + host + "`)", Service: name}
		if c.TLS {
			router.TLS = &httpRouterTLS{CertResolver: c.CertResolver}
		}
		config.HTTP.Routers[name] = router
		config.HTTP.Services[name] = httpService{LoadBalancer: httpLoadBalancer{
			Servers: []httpBackend{{URL: target}}, PassHostHeader: true,
			HealthCheck: httpCheck{Path: healthPath, Interval: "5s", Timeout: "2s"},
		}}
	}
	addHTTP("kratos-registry", "registry."+c.Domain, c.HubURL, "/healthz")
	if status.Files {
		addHTTP("kratos-files", "files."+c.Domain, c.HubURL, "/healthz")
	}
	var primaries []NodeInfo
	for _, node := range nodes {
		if node.ID == c.MinioNodeID && node.MinioHealthy && validHTTPURL(node.MinioURL) {
			addHTTP("kratos-s3", "s3."+c.Domain, node.MinioURL, "/minio/health/live")
			status.MinioNode, status.MinioState = node.ID, "ready"
			if validHTTPURL(node.MinioConsoleURL) {
				addHTTP("kratos-console", "console."+c.Domain, node.MinioConsoleURL, "/")
			}
		}
		if node.PostgresHealthy && node.PostgresRole == "primary" && validPostgresAddress(node.PostgresAddress) {
			primaries = append(primaries, node)
		}
	}
	if len(primaries) == 1 {
		node := primaries[0]
		config.TCP.Routers["kratos-postgres"] = tcpRouter{EntryPoints: []string{c.PostgresEntryPoint}, Rule: "HostSNI(`*`)", Service: "kratos-postgres"}
		config.TCP.Services["kratos-postgres"] = tcpService{LoadBalancer: tcpLoadBalancer{Servers: []tcpBackend{{Address: node.PostgresAddress}}}}
		status.PostgresNode, status.PostgresState = node.ID, "ready"
	} else if len(primaries) > 1 {
		status.PostgresState = "multiple_primaries_blocked"
	}
	return config, status
}

func (h *Hub) traefik(w http.ResponseWriter, _ *http.Request) {
	if h.gateway.HubURL == "" {
		writeError(w, 503, "configure GATEWAY_HUB_URL to enable the provider")
		return
	}
	config, _ := h.gatewaySnapshot()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, config)
}

func (h *Hub) gatewayStatus(w http.ResponseWriter, _ *http.Request) {
	if h.gateway.HubURL == "" {
		writeError(w, 503, "gateway provider is disabled")
		return
	}
	_, status := h.gatewaySnapshot()
	writeJSON(w, 200, status)
}

func validPostgresAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	if strings.ContainsAny(host, " \t\r\n/\\") {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

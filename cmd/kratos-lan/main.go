package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	kratos "github.com/go-kratos/kratos/v3"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"kratos-lan/internal/lan"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	role := strings.ToLower(env("ROLE", "hub"))
	token := os.Getenv("LAN_TOKEN")
	if len(token) < 24 {
		return lan.ErrShortToken
	}
	addr := env("HTTP_ADDR", ":8080")
	srv := khttp.NewServer(khttp.Address(addr))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch role {
	case "hub":
		hub, err := lan.NewHub(token, os.Getenv("ADVERTISE_URL"))
		if err != nil {
			return err
		}
		if gatewayHubURL := env("GATEWAY_HUB_URL", os.Getenv("ADVERTISE_URL")); gatewayHubURL != "" {
			tlsEnabled, err := strconv.ParseBool(env("GATEWAY_TLS", "false"))
			if err != nil {
				return err
			}
			if err := hub.ConfigureGateway(lan.GatewayConfig{
				HubURL: gatewayHubURL, Domain: env("GATEWAY_DOMAIN", "kratos.internal"),
				HTTPEntryPoint: env("GATEWAY_HTTP_ENTRYPOINT", "web"), PostgresEntryPoint: env("GATEWAY_POSTGRES_ENTRYPOINT", "postgres"),
				MinioNodeID: env("GATEWAY_MINIO_NODE_ID", "storage-1"), TLS: tlsEnabled, CertResolver: os.Getenv("GATEWAY_CERT_RESOLVER"),
			}); err != nil {
				return err
			}
		}
		srv.HandlePrefix("/", hub)
		go hub.RunDiscovery(ctx, env("DISCOVERY_ADDR", ":39471"))
	case "node":
		replicas, err := strconv.Atoi(env("REPLICA_COUNT", "1"))
		if err != nil {
			return err
		}
		maxBytes, err := strconv.ParseInt(env("MAX_FILE_BYTES", "10737418240"), 10, 64)
		if err != nil {
			return err
		}
		node, err := lan.NewNode(lan.NodeConfig{
			ID: os.Getenv("NODE_ID"), Token: token,
			DataDir: env("DATA_DIR", "/data"), FileURL: os.Getenv("FILE_URL"),
			MinioURL: os.Getenv("MINIO_URL"), PostgresAddress: os.Getenv("POSTGRES_ADDRESS"),
			MinioConsoleURL: os.Getenv("MINIO_CONSOLE_URL"),
			PostgresProbe:   lan.PostgresProbe{User: os.Getenv("POSTGRES_PROBE_USER"), Password: os.Getenv("POSTGRES_PROBE_PASSWORD"), Database: env("POSTGRES_PROBE_DATABASE", "app"), SSLMode: env("POSTGRES_PROBE_SSLMODE", "prefer")},
			HubURL:          os.Getenv("HUB_URL"), DiscoveryPort: env("DISCOVERY_PORT", "39471"),
			ReplicaCount: replicas, MaxFileBytes: maxBytes,
		})
		if err != nil {
			return err
		}
		srv.HandlePrefix("/", node)
		go node.RunRegistration(ctx)
	default:
		return lan.ErrInvalidRole
	}
	app := kratos.New(kratos.Name("kratos-lan-"+role), kratos.Server(srv), kratos.Context(ctx))
	return app.Run()
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

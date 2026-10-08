package lan

import (
	"context"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

type PostgresProbe struct {
	User, Password, Database, SSLMode string
}

// A failed or unconfigured SQL probe never reports a writable primary.
func probePostgres(ctx context.Context, address string, p PostgresProbe) (bool, string) {
	if address == "" || p.User == "" || p.Database == "" {
		return false, "unknown"
	}
	if p.SSLMode == "" {
		p.SSLMode = "prefer"
	}
	endpoint := url.URL{Scheme: "postgres", Host: address, User: url.UserPassword(p.User, p.Password), Path: "/" + p.Database}
	endpoint.RawQuery = url.Values{"sslmode": []string{p.SSLMode}}.Encode()
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(checkCtx, endpoint.String())
	if err != nil {
		return false, "unknown"
	}
	defer conn.Close(checkCtx)
	var recovery bool
	if err := conn.QueryRow(checkCtx, "SELECT pg_is_in_recovery()").Scan(&recovery); err != nil {
		return false, "unknown"
	}
	if recovery {
		return true, "standby"
	}
	return true, "primary"
}

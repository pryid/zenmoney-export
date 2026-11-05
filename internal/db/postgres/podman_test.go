package postgres

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// startTestPostgres runs an isolated database directly with Podman, without a Docker API service.
func startTestPostgres(t *testing.T, ctx context.Context, database string) string {
	t.Helper()
	version := os.Getenv("POSTGRES_VERSION")
	if version == "" {
		version = "18"
	}
	cmd := exec.CommandContext(ctx, "podman", "run", "-d", "--rm",
		"-p", "127.0.0.1::5432",
		"-e", "POSTGRES_USER="+database,
		"-e", "POSTGRES_PASSWORD="+database,
		"-e", "POSTGRES_DB="+database,
		"docker.io/library/postgres:"+version+"-alpine")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	require.NoErrorf(t, err, "start PostgreSQL with Podman: %s", stderr.String())
	id := strings.TrimSpace(string(output))
	require.NotEmpty(t, id)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := exec.CommandContext(cleanupCtx, "podman", "rm", "-f", id).CombinedOutput()
		if err != nil {
			t.Errorf("remove test PostgreSQL container: %v: %s", err, output)
		}
	})

	output, err = exec.CommandContext(ctx, "podman", "port", id, "5432/tcp").CombinedOutput()
	require.NoErrorf(t, err, "get PostgreSQL port: %s", output)
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	dsn := (&url.URL{
		Scheme: "postgres", User: url.UserPassword(database, database),
		Host: net.JoinHostPort("127.0.0.1", port), Path: database, RawQuery: "sslmode=disable",
	}).String()

	var lastErr error
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			_ = conn.Close(context.Background())
			if err == nil {
				return dsn
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.FailNow(t, "PostgreSQL did not become ready", fmt.Sprint(lastErr))
	return ""
}

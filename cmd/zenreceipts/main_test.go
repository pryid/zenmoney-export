package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestFetchReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var request map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "q=example&x=\"test\"", request["code"])
		_, _ = io.WriteString(
			w,
			`{"date":"2026-11-05","items":[{"name":"Milk","quantity":1.25,"price":12.34,"sum":15.425}]}`,
		)
	}))
	defer server.Close()
	client := receiptClient{httpClient: server.Client(), endpoint: server.URL, token: "test-token"}
	raw, response, err := client.fetch(context.Background(), `q=example&x="test"`)
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"date":"2026-11-05","items":[{"name":"Milk","quantity":1.25,"price":12.34,"sum":15.425}]}`,
		string(raw),
	)
	require.Equal(
		t,
		[]receiptItem{{Name: "Milk", Quantity: "1.25", Price: "12.34", Sum: "15.425"}},
		response.Items,
	)
}

func TestFetchReceiptHTTPFailureAndUnavailable(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{name: "rate limit stops fetch", status: http.StatusTooManyRequests, wantError: true},
		{name: "authorization fails", status: http.StatusUnauthorized, wantError: true},
		{name: "not found is unavailable", status: http.StatusNotFound},
		{name: "bad receipt is unavailable", status: http.StatusBadRequest},
		{name: "invalid receipt is unavailable", status: http.StatusUnprocessableEntity},
		{name: "empty items are unavailable", status: http.StatusOK, body: `{"items":[]}`},
		{name: "invalid JSON fails", status: http.StatusOK, body: `not json`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
				}),
			)
			defer server.Close()
			client := receiptClient{
				httpClient: server.Client(),
				endpoint:   server.URL,
				token:      "secret-token",
			}
			_, result, err := client.fetch(context.Background(), "secret-qr")
			if test.wantError {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-token")
				require.NotContains(t, err.Error(), "secret-qr")
			} else {
				require.NoError(t, err)
				require.Empty(t, result.Items)
			}
		})
	}
}

func TestFetchRetriesServerErrors(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"name":"Tea","quantity":1,"price":2,"sum":2}]}`)
	}))
	defer server.Close()
	client := receiptClient{
		httpClient:  server.Client(),
		endpoint:    server.URL,
		token:       "test",
		retryDelays: []time.Duration{0, 0},
	}
	_, receipt, err := client.fetchWithRetry(context.Background(), "test-code")
	require.NoError(t, err)
	require.Len(t, receipt.Items, 1)
	require.Equal(t, 3, calls)
}

func TestReceiptPersistenceAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := startPostgres(t, ctx)
	migrationDir := filepath.Join("..", "..", "migrations", "postgres")
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	require.NoError(t, err)
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		_, err = db.Exec(ctx, string(data))
		require.NoErrorf(t, err, "apply %s", filepath.Base(file))
	}
	_, err = db.Exec(ctx, `INSERT INTO transaction (id, date, qr_code, deleted)
		VALUES ('00000000-0000-0000-0000-000000000001', '2026-11-04', 'qr-1', false),
		       ('00000000-0000-0000-0000-000000000002', '2026-11-04', 'qr-1', false),
		       ('00000000-0000-0000-0000-000000000003', '2026-11-03', 'qr-2', false),
		       ('00000000-0000-0000-0000-000000000004', '2026-11-02', 'qr-deleted', true)`)
	require.NoError(t, err)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		if request["code"] == "qr-3" || strings.HasPrefix(request["code"], "bad-") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if request["code"] == "qr-2" && requests <= 2 {
			_, _ = io.WriteString(w, `{"items":[]}`)
			return
		}
		_, _ = io.WriteString(
			w,
			`{"date":"2026-11-04","payee":"Store","items":[{"name":"Milk","quantity":1.25,"price":12.34,"sum":15.425}]}`,
		)
	}))
	defer server.Close()
	client := receiptClient{httpClient: server.Client(), endpoint: server.URL, token: "test"}
	var log bytes.Buffer
	require.NoError(t, fetchReceipts(ctx, db, client, 0, 0, &log))
	require.Equal(t, 2, requests, "duplicate QR and deleted transaction must not be fetched")
	var count int
	require.NoError(t, db.QueryRow(ctx, "SELECT COUNT(*) FROM receipt_item").Scan(&count))
	require.Equal(t, 1, count)
	var amount string
	require.NoError(t, db.QueryRow(ctx, "SELECT amount::text FROM receipt_item").Scan(&amount))
	require.Equal(t, "15.425", amount)
	// An invalid replacement must roll back both the receipt and its original items.
	err = saveReceipt(
		ctx,
		db,
		"qr-1",
		[]byte(`{"items":[]}`),
		[]receiptItem{{Name: "bad", Price: "not-a-number"}},
		"",
	)
	require.Error(t, err)
	require.NoError(t, db.QueryRow(ctx, "SELECT amount::text FROM receipt_item").Scan(&amount))
	require.Equal(t, "15.425", amount)
	var output bytes.Buffer
	require.NoError(t, exportCSV(ctx, db, &output))
	csvRows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	require.NoError(t, err)
	require.Len(t, csvRows, 2)
	require.Equal(
		t,
		[]string{"2026-11-04", "Store", "0", "Milk", "1.25", "12.34", "15.425"},
		csvRows[1][1:],
	)
	require.NoError(t, fetchReceipts(ctx, db, client, 0, 0, &log))
	require.Equal(t, 3, requests, "only previously unavailable receipts must be retried")
	require.NoError(t, db.QueryRow(ctx, "SELECT COUNT(*) FROM receipt_item").Scan(&count))
	require.Equal(t, 2, count)

	_, err = db.Exec(ctx, `INSERT INTO transaction (id, date, qr_code, deleted)
		VALUES ('00000000-0000-0000-0000-000000000005', '2026-11-06', 'qr-3', false),
		       ('00000000-0000-0000-0000-000000000006', '2026-11-05', 'qr-4', false)`)
	require.NoError(t, err)
	client.retryDelays = []time.Duration{0, 0}
	require.NoError(t, fetchReceipts(ctx, db, client, 0, 0, &log))
	require.Equal(
		t,
		7,
		requests,
		"failed receipt should receive three attempts before the next receipt",
	)
	var status, lastError string
	require.NoError(
		t,
		db.QueryRow(ctx, `SELECT status, last_error FROM receipt WHERE qr_code = 'qr-3'`).
			Scan(&status, &lastError),
	)
	require.Equal(t, "unavailable", status)
	require.Equal(t, "HTTP 500", lastError)
	require.NoError(t, db.QueryRow(ctx, "SELECT COUNT(*) FROM receipt_item").Scan(&count))
	require.Equal(t, 3, count, "the receipt following a failure must still be saved")

	// A widespread outage must stop the run rather than querying every remaining receipt.
	_, err = db.Exec(ctx, "TRUNCATE receipt_item, receipt, transaction RESTART IDENTITY")
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO transaction (id, date, qr_code, deleted)
		VALUES ('00000000-0000-0000-0000-000000000011', '2026-11-04', 'bad-1', false),
		       ('00000000-0000-0000-0000-000000000012', '2026-11-03', 'bad-2', false),
		       ('00000000-0000-0000-0000-000000000013', '2026-11-02', 'bad-3', false),
		       ('00000000-0000-0000-0000-000000000014', '2026-11-01', 'untouched', false)`)
	require.NoError(t, err)
	requests = 0
	err = fetchReceipts(ctx, db, client, 0, 0, &log)
	require.ErrorContains(t, err, "3 consecutive")
	require.Equal(t, 9, requests, "three failures with three attempts each must stop the run")
	require.NoError(
		t,
		db.QueryRow(ctx, "SELECT COUNT(*) FROM receipt WHERE last_error = 'HTTP 500'").Scan(&count),
	)
	require.Equal(t, 3, count)
}

func startPostgres(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	cmd := exec.CommandContext(
		ctx,
		"podman",
		"run",
		"-d",
		"--rm",
		"-p",
		"127.0.0.1::5432",
		"-e",
		"POSTGRES_PASSWORD=receipts-test",
		"docker.io/library/postgres:18-alpine",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	require.NoErrorf(t, err, "start PostgreSQL: %s", stderr.String())
	id := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := exec.CommandContext(cleanup, "podman", "rm", "-f", id).CombinedOutput()
		if err != nil {
			t.Errorf("remove PostgreSQL: %v: %s", err, output)
		}
	})
	output, err = exec.CommandContext(ctx, "podman", "port", id, "5432/tcp").CombinedOutput()
	require.NoErrorf(t, err, "get PostgreSQL port: %s", output)
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	dsn := fmt.Sprintf(
		"postgres://postgres:receipts-test@127.0.0.1:%s/postgres?sslmode=disable",
		port,
	)
	for ctx.Err() == nil {
		config, err := pgx.ParseConfig(dsn)
		require.NoError(t, err)
		config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		db, err := pgx.ConnectConfig(ctx, config)
		if err == nil {
			t.Cleanup(func() { _ = db.Close(context.Background()) })
			return db
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("PostgreSQL did not become ready")
	return nil
}

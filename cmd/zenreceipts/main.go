// zenreceipts fetches receipt line items from ZenMoney into PostgreSQL.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	receiptEndpoint  = "https://api.zenmoney.ru/parse_qr_code/"
	maxResponseBytes = 4 << 20
	receiptLockID    = int64(9020311830762)
)

type receiptItem struct {
	Name     string      `json:"name"`
	Quantity json.Number `json:"quantity"`
	Price    json.Number `json:"price"`
	Sum      json.Number `json:"sum"`
}

type receiptResponse struct {
	Items []receiptItem `json:"items"`
}

type receiptClient struct {
	httpClient  *http.Client
	endpoint    string
	token       string
	retryDelays []time.Duration
}

type receiptHTTPError struct {
	status int
}

func (e *receiptHTTPError) Error() string {
	return fmt.Sprintf("receipt request returned HTTP %d", e.status)
}

func (c receiptClient) fetch(ctx context.Context, qr string) ([]byte, *receiptResponse, error) {
	body, err := json.Marshal(map[string]string{"code": qr})
	if err != nil {
		return nil, nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("receipt request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, &receiptResponse{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &receiptHTTPError{status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read receipt response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, nil, errors.New("receipt response exceeds size limit")
	}
	var result receiptResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("decode receipt response: %w", err)
	}
	return raw, &result, nil
}

// fetchWithRetry retries server errors, while authorization and rate-limit failures stop immediately.
func (c receiptClient) fetchWithRetry(
	ctx context.Context,
	qr string,
) ([]byte, *receiptResponse, error) {
	for attempt := 0; ; attempt++ {
		raw, response, err := c.fetch(ctx, qr)
		var statusErr *receiptHTTPError
		if err == nil || !errors.As(err, &statusErr) || statusErr.status < 500 ||
			statusErr.status > 599 ||
			attempt >= len(c.retryDelays) {
			return raw, response, err
		}
		timer := time.NewTimer(c.retryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, log io.Writer) error {
	if len(args) == 0 || (args[0] != "fetch" && args[0] != "export") {
		return errors.New("usage: zenreceipts fetch [--limit N] [--interval 1s] | export")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(log)
	limit := flags.Int("limit", 0, "maximum number of receipts to request (0: all)")
	interval := flags.Duration("interval", time.Second, "minimum interval between requests")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *limit < 0 || *interval < 0 {
		return errors.New("invalid arguments: limit and interval must be non-negative")
	}
	dsn := os.Getenv("DB_URL")
	if dsn == "" {
		return errors.New("DB_URL is required")
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close(context.Background())
	if args[0] == "export" {
		return exportCSV(ctx, db, out)
	}
	token := os.Getenv("ZEN_API_TOKEN")
	if token == "" {
		return errors.New("ZEN_API_TOKEN is required")
	}
	client := receiptClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("receipt endpoint redirect refused")
			},
		},
		endpoint:    receiptEndpoint,
		token:       token,
		retryDelays: []time.Duration{2 * time.Second, 4 * time.Second},
	}
	return fetchReceipts(ctx, db, client, *limit, *interval, log)
}

func fetchReceipts(
	ctx context.Context,
	db *pgx.Conn,
	client receiptClient,
	limit int,
	interval time.Duration,
	log io.Writer,
) error {
	var locked bool
	if err := db.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", receiptLockID).
		Scan(&locked); err != nil {
		return fmt.Errorf("acquire receipt lock: %w", err)
	}
	if !locked {
		return errors.New("another receipt fetch is already running")
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", receiptLockID)
	}()
	rows, err := db.Query(ctx, `
		SELECT t.qr_code FROM transaction t
		LEFT JOIN receipt r ON r.qr_code = t.qr_code
		WHERE NOT COALESCE(t.deleted, false) AND NULLIF(BTRIM(t.qr_code), '') IS NOT NULL
		  AND (r.status IS NULL OR r.status <> 'complete')
		GROUP BY t.qr_code ORDER BY MAX(t.date) DESC, t.qr_code
		LIMIT NULLIF($1, 0)`, limit)
	if err != nil {
		return fmt.Errorf("list receipts (run the database migrations first): %w", err)
	}
	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			rows.Close()
			return fmt.Errorf("read receipt identifier: %w", err)
		}
		codes = append(codes, code)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read receipt identifiers: %w", err)
	}
	fmt.Fprintf(log, "Receipts to request: %d\n", len(codes))
	complete, unavailable, consecutiveErrors := 0, 0, 0
	for i, code := range codes {
		if i > 0 && interval > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		raw, response, err := client.fetchWithRetry(ctx, code)
		lastError := ""
		var statusErr *receiptHTTPError
		if err != nil {
			if !errors.As(err, &statusErr) || statusErr.status < 500 || statusErr.status > 599 {
				return fmt.Errorf("stopped after %d/%d receipts: %w", i, len(codes), err)
			}
			lastError = fmt.Sprintf("HTTP %d", statusErr.status)
			response = &receiptResponse{}
			consecutiveErrors++
		} else {
			consecutiveErrors = 0
		}
		if err := saveReceipt(ctx, db, code, raw, response.Items, lastError); err != nil {
			return fmt.Errorf("save receipt %d/%d: %w", i+1, len(codes), err)
		}
		if len(response.Items) == 0 {
			unavailable++
		} else {
			complete++
		}
		if lastError != "" {
			fmt.Fprintf(
				log,
				"Receipt %d/%d unavailable after retries (%s); continuing\n",
				i+1,
				len(codes),
				lastError,
			)
		}
		if consecutiveErrors >= 3 {
			return fmt.Errorf(
				"stopped after %d consecutive receipt server errors; retry later",
				consecutiveErrors,
			)
		}
		if (i+1)%10 == 0 || i+1 == len(codes) {
			fmt.Fprintf(
				log,
				"Processed: %d/%d, with items: %d, unavailable: %d\n",
				i+1,
				len(codes),
				complete,
				unavailable,
			)
		}
	}
	return nil
}

func saveReceipt(
	ctx context.Context,
	db *pgx.Conn,
	qr string,
	raw []byte,
	items []receiptItem,
	lastError string,
) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	status := "complete"
	if len(items) == 0 {
		status = "unavailable"
	}
	var response any
	if len(raw) > 0 {
		response = string(raw)
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO receipt (qr_code, status, response, last_error, fetched_at)
		VALUES ($1, $2, $3::jsonb, NULLIF($4, ''), CURRENT_TIMESTAMP)
		ON CONFLICT (qr_code) DO UPDATE SET status = EXCLUDED.status,
		response = EXCLUDED.response, last_error = EXCLUDED.last_error, fetched_at = EXCLUDED.fetched_at
		RETURNING id`, qr, status, response, lastError).Scan(&id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM receipt_item WHERE receipt_id = $1", id); err != nil {
		return err
	}
	for i, item := range items {
		_, err = tx.Exec(
			ctx,
			`INSERT INTO receipt_item (receipt_id, position, name, quantity, price, amount)
			VALUES ($1, $2, $3, ($4::text)::numeric, ($5::text)::numeric, ($6::text)::numeric)`,
			id,
			i,
			item.Name,
			numericValue(item.Quantity),
			numericValue(item.Price),
			numericValue(item.Sum),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func numericValue(value json.Number) any {
	if value == "" {
		return nil
	}
	return value.String()
}

func exportCSV(ctx context.Context, db *pgx.Conn, output io.Writer) error {
	rows, err := db.Query(ctx, `SELECT r.id::text, COALESCE(r.response->>'date', ''),
		COALESCE(r.response->>'payee', ''), i.position::text, i.name,
		COALESCE(i.quantity::text, ''), COALESCE(i.price::text, ''), COALESCE(i.amount::text, '')
		FROM receipt r JOIN receipt_item i ON i.receipt_id = r.id
		WHERE r.status = 'complete' ORDER BY r.id, i.position`)
	if err != nil {
		return fmt.Errorf("read items (run the database migrations first): %w", err)
	}
	defer rows.Close()
	writer := csv.NewWriter(output)
	if err := writer.Write(
		[]string{"receipt_id", "date", "payee", "position", "name", "quantity", "price", "amount"},
	); err != nil {
		return err
	}
	for rows.Next() {
		values := make([]string, 8)
		args := make([]any, len(values))
		for i := range values {
			args[i] = &values[i]
		}
		if err := rows.Scan(args...); err != nil {
			return err
		}
		if err := writer.Write(values); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

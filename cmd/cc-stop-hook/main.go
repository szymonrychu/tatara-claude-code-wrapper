package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cc-stop-hook:", err)
	}
	os.Exit(0) // never block or alter claude
}

func run() error {
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	internalURL := envOr("CCW_INTERNAL_URL", "http://127.0.0.1:8090/internal/turn-complete")
	resultPath := envOr("CCW_RESULT_JSON", "/workspace/result.json")

	res, err := buildResult(payload, resultPath)
	if err != nil {
		return err
	}
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, internalURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

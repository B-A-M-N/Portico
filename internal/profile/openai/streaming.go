package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// VerifyStreaming sends a minimal streaming chat request and verifies the
// wire contract: event-stream content type, at least one data chunk, and the
// terminal [DONE] marker. Reading incrementally through bufio.Reader makes a
// gateway that buffers the entire response fail this check in live use.
func VerifyStreaming(ctx context.Context, baseURL, authToken, model string) error {
	baseURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return err
	}
	if model == "" {
		model = "test"
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinPath(baseURL, "/v1/chat/completions"), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("streaming chat returned status %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("streaming chat returned content type %q", resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	chunks := 0
	done := false
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				done = true
				break
			}
			if data != "" {
				chunks++
			}
		}
		if err != nil {
			if err == io.EOF && done {
				break
			}
			return fmt.Errorf("read streaming chat: %w", err)
		}
	}
	if chunks == 0 {
		return fmt.Errorf("streaming chat produced no SSE data chunks")
	}
	if !done {
		return fmt.Errorf("streaming chat did not terminate with [DONE]")
	}
	return nil
}

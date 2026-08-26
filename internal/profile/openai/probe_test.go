package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeOpenAICompatibilityAndStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"tiny"}]}`)
		case "/v1/chat/completions":
			if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, _ := w.(http.Flusher)
				fmt.Fprintln(w, "data: chunk")
				if flusher != nil {
					flusher.Flush()
				}
				fmt.Fprintln(w, "data: [DONE]")
				return
			}
			fmt.Fprint(w, `{"id":"chatcmpl"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	compat, err := ProbeOpenAICompatibility(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("ProbeOpenAICompatibility: %v", err)
	}
	if len(compat.Models) != 1 || !compat.ChatCompletions.Supported {
		t.Fatalf("compatibility = %+v", compat)
	}
	if err := VerifyStreaming(context.Background(), server.URL, "", "tiny"); err != nil {
		t.Fatalf("VerifyStreaming: %v", err)
	}
}

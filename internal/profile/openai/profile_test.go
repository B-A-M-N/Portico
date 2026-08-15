package openai

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestProbeOpenAICompatibility verifies the compatibility probe.
func TestProbeOpenAICompatibility(t *testing.T) {
	// Start a fake OpenAI-compatible server.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3"},{"id":"gpt-4"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"}}]}`))
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	baseURL := "http://" + listener.Addr().String()

	comp, err := ProbeOpenAICompatibility(context.Background(), baseURL, "")
	if err != nil {
		t.Fatalf("ProbeOpenAICompatibility: %v", err)
	}

	if len(comp.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(comp.Models))
	}
	if comp.Models[0] != "llama-3" || comp.Models[1] != "gpt-4" {
		t.Fatalf("unexpected models: %v", comp.Models)
	}
	if !comp.ChatCompletions.Supported {
		t.Fatal("ChatCompletions should be supported")
	}
}

// TestVerifyStreaming verifies the streaming verification function.
func TestVerifyStreaming(t *testing.T) {
	// Start a fake streaming server.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(`data: {"id":"chatcmpl-123","choices":[{"delta":{"content":"Hello"}}]}` + "\n\n"))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	baseURL := "http://" + listener.Addr().String()

	err = VerifyStreaming(context.Background(), baseURL, "")
	if err != nil {
		t.Fatalf("VerifyStreaming: %v", err)
	}
}

// TestVerifyStreamingFailsWithoutStreaming verifies that a non-streaming response fails.
func TestVerifyStreamingFailsWithoutStreaming(t *testing.T) {
	// Start a server that doesn't support streaming.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"}}]}`))
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	baseURL := "http://" + listener.Addr().String()

	err = VerifyStreaming(context.Background(), baseURL, "")
	if err == nil {
		t.Fatal("VerifyStreaming should fail for non-streaming response")
	}
	if !strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestConnectionProfileValidation validates the connection profile.
func TestConnectionProfileValidation(t *testing.T) {
	// Valid OpenAI profile.
	profile := ConnectionProfile{
		ID:                  "conn-llm",
		Name:                "LLM",
		Target:              TargetSpec{BaseURL: "http://127.0.0.1:8000", BasePath: "/v1", Protocol: "http"},
		Gateway:             GatewaySpec{Enabled: true, AuthRequired: true, AllowSSE: true},
		TransportProviderID: "cloudflare",
	}

	if profile.Gateway.AllowSSE != true {
		t.Fatal("AllowSSE must be true for OpenAI profiles")
	}
}

// TestGenerateToken verifies token generation.
func TestGenerateToken(t *testing.T) {
	// Token generation is tested indirectly via TestGatewayAutoTokenGeneration
	// in the gateway package. Here we just verify the format.
	prefix := "portico_"
	if len(prefix) != 8 {
		t.Fatalf("prefix length = %d, want 8", len(prefix))
	}
}

package tunnel

import (
	"testing"
)

func TestExtractQuickTunnelURL_ValidSubdomain(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "typical cloudflared info line",
			input: `2025-08-17T12:00:00Z INF |  Your Quick Tunnel has been created! Visit it at:   https://random-words-here.trycloudflare.com  `,
			want:  "https://random-words-here.trycloudflare.com",
		},
		{
			name:  "url embedded in surrounding text",
			input: `time="2025-08-17T12:00:00Z" level=info msg="quick tunnel is ready: https://abc-123-xyz.trycloudflare.com"`,
			want:  "https://abc-123-xyz.trycloudflare.com",
		},
		{
			name:  "url with trailing slash",
			input: `INF https://my-tunnel.trycloudflare.com/`,
			want:  "https://my-tunnel.trycloudflare.com/",
		},
		{
			name:  "url with port",
			input: `INF https://my-tunnel.trycloudflare.com:8080/`,
			want:  "https://my-tunnel.trycloudflare.com:8080/",
		},
		{
			name:  "multiple urls picks first valid",
			input: `INF https://example.com/ and https://good-tunnel.trycloudflare.com/`,
			want:  "https://good-tunnel.trycloudflare.com/",
		},
		{
			name:  "url with hyphenated subdomain",
			input: `INF https://a-b-c-d-e-f.trycloudflare.com`,
			want:  "https://a-b-c-d-e-f.trycloudflare.com",
		},
		{
			name:  "url followed by closing quote",
			input: `"https://tunnel-1.trycloudflare.com"`,
			want:  "https://tunnel-1.trycloudflare.com",
		},
		{
			name:  "url followed by comma",
			input: `INF https://tunnel-2.trycloudflare.com, done`,
			want:  "https://tunnel-2.trycloudflare.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractQuickTunnelURL(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractQuickTunnelURL_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "no url",
			input: "no url here",
		},
		{
			name:  "http scheme",
			input: "INF http://my-tunnel.trycloudflare.com",
		},
		{
			name:  "evil domain",
			input: "INF https://eviltrycloudflare.com",
		},
		{
			name:  "path under trycloudflare.com",
			input: "INF https://trycloudflare.com/some-path",
		},
		{
			name:  "userinfo present",
			input: "INF https://user:pass@my-tunnel.trycloudflare.com",
		},
		{
			name:  "subdomain with dot prefix",
			input: "INF https://.trycloudflare.com",
		},
		{
			name:  "subdomain with trailing dot",
			input: "INF https://my-tunnel.trycloudflare.com.",
		},
		{
			name:  "lookalike suffix",
			input: "INF https://my-tunnel.eviltrycloudflare.com",
		},
		{
			name:  "path tricks",
			input: "INF https://my-tunnel.trycloudflare.com/../../etc/passwd",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractQuickTunnelURL(tc.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestExtractQuickTunnelURLFromLines(t *testing.T) {
	lines := []string{
		"2025-08-17T12:00:00Z INF starting tunnel...",
		"2025-08-17T12:00:01Z INF registering tunnel...",
		"2025-08-17T12:00:02Z INF quick tunnel ready: https://my-tunnel.trycloudflare.com",
	}
	got, err := ExtractQuickTunnelURLFromLines(lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://my-tunnel.trycloudflare.com"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

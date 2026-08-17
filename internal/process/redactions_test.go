package process

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

func TestProcessLogRedactions_RedactsFlagValuePairs(t *testing.T) {
	cases := []struct {
		name   string
		spec   core.ProcessSpec
		want   []string
	}{
		{
			name: "token-file value form",
			spec: core.ProcessSpec{
				Args: []string{"tunnel", "run", "--token-file", "/secret/path/token.json"},
			},
			want: []string{"/secret/path/token.json"},
		},
		{
			name: "token-file=value form",
			spec: core.ProcessSpec{
				Args: []string{"tunnel", "run", "--token-file=/secret/path/token.json"},
			},
			want: []string{"/secret/path/token.json"},
		},
		{
			name: "api-key value form",
			spec: core.ProcessSpec{
				Args: []string{"agent", "--api-key", "ak_live_12345"},
			},
			want: []string{"ak_live_12345"},
		},
		{
			name: "multiple secrets",
			spec: core.ProcessSpec{
				Args: []string{"run", "--token-file", "/t", "--api-key", "ak"},
			},
			want: []string{"/t", "ak"},
		},
		{
			name: "non-secret flag is not redacted",
			spec: core.ProcessSpec{
				Args: []string{"run", "--log-level", "debug"},
			},
			want: nil,
		},
		{
			name: "explicit redactions preserved",
			spec: core.ProcessSpec{
				Redactions: []string{"explicit-secret"},
			},
			want: []string{"explicit-secret"},
		},
		{
			name: "ngrok env var redacted",
			spec: core.ProcessSpec{
				Env: []string{"NGROK_AUTHTOKEN=ngok-secret-token"},
			},
			want: []string{"ngok-secret-token"},
		},
		{
			name: "cloudflare env var redacted",
			spec: core.ProcessSpec{
				Env: []string{"CF_API_TOKEN=cf-secret-token"},
			},
			want: []string{"cf-secret-token"},
		},
		{
			name: "tunnel token env var redacted",
			spec: core.ProcessSpec{
				Env: []string{"TUNNEL_TOKEN_FILE=/secret/path"},
			},
			want: []string{"/secret/path"},
		},
		{
			name: "control plane api key redacted",
			spec: core.ProcessSpec{
				Env: []string{"CONTROL_PLANE_API_KEY=cp-key"},
			},
			want: []string{"cp-key"},
		},
		{
			name: "non-secret env var not redacted",
			spec: core.ProcessSpec{
				Env: []string{"PATH=/usr/bin", "HOME=/home/user"},
			},
			want: nil,
		},
		{
			name: "combined: flag + env + explicit",
			spec: core.ProcessSpec{
				Args:       []string{"run", "--token-file", "/tf"},
				Env:        []string{"NGROK_AUTHTOKEN=nt"},
				Redactions: []string{"explicit"},
			},
			want: []string{"explicit", "/tf", "nt"},
		},
		{
			name: "duplicate values deduped",
			spec: core.ProcessSpec{
				Redactions: []string{"same"},
				Env:        []string{"NGROK_AUTHTOKEN=same"},
			},
			want: []string{"same"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := processLogRedactions(tc.spec)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

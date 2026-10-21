package main

import (
	"reflect"
	"testing"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8765": true,
		"127.0.0.1:0":    true,
		"[::1]:8765":     true,
		"localhost:8765": true,
		"0.0.0.0:8765":   false,
		"192.168.1.1:80": false,
		"10.0.0.1:8765":  false,
	}
	for addr, want := range cases {
		t.Run(addr, func(t *testing.T) {
			if got := isLoopbackAddr(addr); got != want {
				t.Fatalf("isLoopbackAddr(%q): want %v, got %v", addr, want, got)
			}
		})
	}
}

func TestResolveAutoDownload(t *testing.T) {
	const mib = 1 << 20
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	cases := []struct {
		name      string
		typesFlag string
		typesSet  bool
		maxFlag   int
		maxSet    bool
		env       map[string]string
		wantTypes []string
		wantMax   uint64
		wantErr   bool
	}{
		{name: "default is off", maxFlag: 25, wantMax: 25 * mib},
		{name: "flag", typesFlag: "image,audio", typesSet: true, maxFlag: 25, wantTypes: []string{"audio", "image"}, wantMax: 25 * mib},
		{name: "env", maxFlag: 25, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all", "WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "8"},
			wantTypes: []string{"audio", "document", "image", "video"}, wantMax: 8 * mib},
		{name: "flag wins over env", typesFlag: "video", typesSet: true, maxFlag: 100, maxSet: true,
			env:       map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all", "WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "8"},
			wantTypes: []string{"video"}, wantMax: 100 * mib},
		{name: "empty flag turns env off", typesSet: true, maxFlag: 25, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all"}, wantMax: 25 * mib},
		{name: "zero means no limit", typesFlag: "document", typesSet: true, maxSet: true, wantTypes: []string{"document"}, wantMax: 0},
		{name: "bad type", typesFlag: "image,sticker", typesSet: true, maxFlag: 25, wantErr: true},
		{name: "bad env type", maxFlag: 25, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "photos"}, wantErr: true},
		{name: "negative flag", typesFlag: "image", typesSet: true, maxFlag: -1, maxSet: true, wantErr: true},
		{name: "non-numeric env", maxFlag: 25, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "25MB"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveAutoDownload(tc.typesFlag, tc.typesSet, tc.maxFlag, tc.maxSet, env(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.TypeList(); len(got)+len(tc.wantTypes) > 0 && !reflect.DeepEqual(got, tc.wantTypes) {
				t.Errorf("types = %v, want %v", got, tc.wantTypes)
			}
			if cfg.MaxBytes != tc.wantMax {
				t.Errorf("MaxBytes = %d, want %d", cfg.MaxBytes, tc.wantMax)
			}
		})
	}
}

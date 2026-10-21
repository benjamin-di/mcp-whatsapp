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
		flags     autoDownloadFlags
		env       map[string]string
		wantTypes []string
		wantMax   uint64
		wantDays  int
		wantErr   bool
	}{
		{name: "default is off", flags: autoDownloadFlags{maxMB: 25}, wantMax: 25 * mib},
		{name: "flag", flags: autoDownloadFlags{types: "image,audio", typesSet: true, maxMB: 25}, wantTypes: []string{"audio", "image"}, wantMax: 25 * mib},
		{name: "env", flags: autoDownloadFlags{maxMB: 25},
			env:       map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all", "WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "8", "WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS": "90"},
			wantTypes: []string{"audio", "document", "image", "video"}, wantMax: 8 * mib, wantDays: 90},
		{name: "flag wins over env", flags: autoDownloadFlags{types: "video", typesSet: true, maxMB: 100, maxMBSet: true, backfillDays: 7, backfillDaysSet: true},
			env:       map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all", "WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "8", "WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS": "90"},
			wantTypes: []string{"video"}, wantMax: 100 * mib, wantDays: 7},
		{name: "empty flag turns env off", flags: autoDownloadFlags{typesSet: true, maxMB: 25}, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "all"}, wantMax: 25 * mib},
		{name: "zero means no limit", flags: autoDownloadFlags{types: "document", typesSet: true, maxMBSet: true}, wantTypes: []string{"document"}, wantMax: 0},
		{name: "zero backfill flag overrides env", flags: autoDownloadFlags{types: "image", typesSet: true, maxMB: 25, backfillDaysSet: true},
			env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS": "30"}, wantTypes: []string{"image"}, wantMax: 25 * mib},
		{name: "bad type", flags: autoDownloadFlags{types: "image,sticker", typesSet: true, maxMB: 25}, wantErr: true},
		{name: "bad env type", flags: autoDownloadFlags{maxMB: 25}, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD": "photos"}, wantErr: true},
		{name: "negative flag", flags: autoDownloadFlags{types: "image", typesSet: true, maxMB: -1, maxMBSet: true}, wantErr: true},
		{name: "non-numeric env", flags: autoDownloadFlags{maxMB: 25}, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB": "25MB"}, wantErr: true},
		{name: "negative backfill days", flags: autoDownloadFlags{types: "image", typesSet: true, maxMB: 25, backfillDays: -1, backfillDaysSet: true}, wantErr: true},
		{name: "non-numeric backfill env", flags: autoDownloadFlags{maxMB: 25}, env: map[string]string{"WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS": "3 months"}, wantErr: true},
		{name: "absurd backfill days", flags: autoDownloadFlags{maxMB: 25, backfillDays: 1 << 40, backfillDaysSet: true}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveAutoDownload(tc.flags, env(tc.env))
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
			if cfg.BackfillDays != tc.wantDays {
				t.Errorf("BackfillDays = %d, want %d", cfg.BackfillDays, tc.wantDays)
			}
		})
	}
}

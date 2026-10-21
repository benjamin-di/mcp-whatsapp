package main

import (
	"flag"
	"io"
	"testing"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

func TestPairingHistoryFlagsResolve(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    client.PairingHistory
		wantErr bool
	}{
		{name: "defaults", want: client.PairingHistory{}},
		{name: "full flag", args: []string{"-full-history"}, want: client.PairingHistory{Full: true}},
		{
			name: "full with limits",
			args: []string{"-full-history", "-history-days", "3650", "-history-size-mb", "2048"},
			want: client.PairingHistory{Full: true, Days: 3650, SizeMB: 2048},
		},
		{
			name: "env only",
			env:  map[string]string{envFullHistory: "1", envHistoryDays: "365", envHistorySizeMB: "512"},
			want: client.PairingHistory{Full: true, Days: 365, SizeMB: 512},
		},
		{
			name: "env true spelled out",
			env:  map[string]string{envFullHistory: "true"},
			want: client.PairingHistory{Full: true},
		},
		{
			name: "flag wins over env",
			args: []string{"-history-days", "30"},
			env:  map[string]string{envFullHistory: "1", envHistoryDays: "365"},
			want: client.PairingHistory{Full: true, Days: 30},
		},
		{
			name: "explicit false flag wins over env",
			args: []string{"-full-history=false"},
			env:  map[string]string{envFullHistory: "1"},
			want: client.PairingHistory{},
		},
		{
			name: "explicit zero flag wins over env",
			args: []string{"-full-history", "-history-days", "0"},
			env:  map[string]string{envHistoryDays: "365"},
			want: client.PairingHistory{Full: true},
		},
		{name: "days without full", args: []string{"-history-days", "30"}, wantErr: true},
		{name: "size env without full", env: map[string]string{envHistorySizeMB: "100"}, wantErr: true},
		{name: "negative days", args: []string{"-full-history", "-history-days", "-1"}, wantErr: true},
		{name: "negative size env", env: map[string]string{envFullHistory: "1", envHistorySizeMB: "-5"}, wantErr: true},
		{name: "days over max", args: []string{"-full-history", "-history-days", "36501"}, wantErr: true},
		{name: "size over max", args: []string{"-full-history", "-history-size-mb", "102401"}, wantErr: true},
		{name: "huge days does not wrap", args: []string{"-full-history", "-history-days", "4294967326"}, wantErr: true},
		{name: "bad bool env", env: map[string]string{envFullHistory: "yes please"}, wantErr: true},
		{name: "bad int env", env: map[string]string{envFullHistory: "1", envHistoryDays: "ten"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			var f pairingHistoryFlags
			f.register(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			getenv := func(k string) string { return tc.env[k] }

			got, err := f.resolve(fs, getenv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolve() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("resolve() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

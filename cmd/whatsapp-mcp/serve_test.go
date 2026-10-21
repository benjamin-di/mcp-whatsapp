package main

import "testing"

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

func TestResolveReadOnly(t *testing.T) {
	cases := []struct {
		name      string
		flagSet   bool
		flagValue bool
		env       string
		want      bool
		wantErr   bool
	}{
		{name: "default off", want: false},
		{name: "env 1", env: "1", want: true},
		{name: "env true", env: "true", want: true},
		{name: "env TRUE", env: "TRUE", want: true},
		{name: "env 0", env: "0", want: false},
		{name: "env false", env: "false", want: false},
		{name: "env typo is an error", env: "yes", wantErr: true},
		{name: "flag on", flagSet: true, flagValue: true, want: true},
		{name: "flag wins over env off", flagSet: true, flagValue: true, env: "0", want: true},
		{name: "flag=false wins over env on", flagSet: true, flagValue: false, env: "1", want: false},
		{name: "flag wins over invalid env", flagSet: true, flagValue: true, env: "yes", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReadOnly(tc.flagSet, tc.flagValue, tc.env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveReadOnly err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("resolveReadOnly = %v, want %v", got, tc.want)
			}
		})
	}
}

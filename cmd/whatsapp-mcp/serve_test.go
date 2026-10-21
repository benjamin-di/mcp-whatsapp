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

func TestResolveKeepRevoked(t *testing.T) {
	cases := []struct {
		name      string
		flagSet   bool
		flagValue bool
		env       string
		want      bool
		wantErr   bool
	}{
		{name: "default off"},
		{name: "env on", env: "1", want: true},
		{name: "env true", env: "true", want: true},
		{name: "env off", env: "0"},
		{name: "flag wins over env", flagSet: true, flagValue: false, env: "1"},
		{name: "flag on", flagSet: true, flagValue: true, want: true},
		{name: "bad env", env: "yes", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveKeepRevoked(tc.flagSet, tc.flagValue, tc.env)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("resolveKeepRevoked(%v, %v, %q) = %v, %v; want %v, err %v",
					tc.flagSet, tc.flagValue, tc.env, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

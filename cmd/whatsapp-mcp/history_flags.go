package main

import (
	"flag"
	"fmt"
	"strconv"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// Environment equivalents of the pairing-history flags. A flag given on the
// command line wins over its variable.
const (
	envFullHistory   = "WHATSAPP_MCP_FULL_HISTORY"
	envHistoryDays   = "WHATSAPP_MCP_HISTORY_DAYS"
	envHistorySizeMB = "WHATSAPP_MCP_HISTORY_SIZE_MB"
)

// pairingHistoryFlags holds the -full-history, -history-days and
// -history-size-mb flags shared by login and serve (both can pair).
type pairingHistoryFlags struct {
	full   bool
	days   int
	sizeMB int
}

func (f *pairingHistoryFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.full, "full-history", false, "when pairing, ask the phone for a full history sync instead of its recent window (env "+envFullHistory+")")
	fs.IntVar(&f.days, "history-days", 0, "with -full-history: days of history to ask for, 0 = phone default (env "+envHistoryDays+")")
	fs.IntVar(&f.sizeMB, "history-size-mb", 0, "with -full-history: megabytes the phone may send, 0 = phone default (env "+envHistorySizeMB+")")
}

// resolve combines the parsed flags with the environment (flag wins) and
// returns validated options. fs must already be parsed.
func (f *pairingHistoryFlags) resolve(fs *flag.FlagSet, getenv func(string) string) (client.PairingHistory, error) {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	full := f.full
	if !set["full-history"] {
		if v := getenv(envFullHistory); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return client.PairingHistory{}, fmt.Errorf("%s=%q: not a boolean", envFullHistory, v)
			}
			full = b
		}
	}

	days, err := resolveLimit("-history-days", f.days, set["history-days"], envHistoryDays, getenv, client.MaxPairingHistoryDays)
	if err != nil {
		return client.PairingHistory{}, err
	}
	sizeMB, err := resolveLimit("-history-size-mb", f.sizeMB, set["history-size-mb"], envHistorySizeMB, getenv, client.MaxPairingHistorySizeMB)
	if err != nil {
		return client.PairingHistory{}, err
	}

	h := client.PairingHistory{Full: full, Days: days, SizeMB: sizeMB}
	if !h.Full && (h.Days != 0 || h.SizeMB != 0) {
		return client.PairingHistory{}, fmt.Errorf("-history-days and -history-size-mb need -full-history (or %s=1)", envFullHistory)
	}
	if err := h.Validate(); err != nil {
		return client.PairingHistory{}, err
	}
	return h, nil
}

// resolveLimit picks the flag value when the flag was given, otherwise the
// environment variable, and checks it lies in [0, max].
func resolveLimit(name string, flagValue int, flagSet bool, env string, getenv func(string) string, max uint32) (uint32, error) {
	n, source := flagValue, name
	if !flagSet {
		v := getenv(env)
		if v == "" {
			return 0, nil
		}
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s=%q: not an integer", env, v)
		}
		n, source = parsed, env
	}
	if n < 0 || n > int(max) {
		return 0, fmt.Errorf("%s must be between 0 and %d, got %d", source, max, n)
	}
	return uint32(n), nil
}

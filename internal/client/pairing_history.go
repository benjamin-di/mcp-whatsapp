package client

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	wmstore "go.mau.fi/whatsmeow/store"
)

// Upper bounds for PairingHistory. They only catch typos and uint32
// wrap-around: the phone applies its own, lower limits regardless.
const (
	MaxPairingHistoryDays   = 36500  // 100 years
	MaxPairingHistorySizeMB = 102400 // 100 GiB
)

// PairingHistory controls how much history this device asks the primary
// phone for when it links. WhatsApp sends history to a companion device only
// during pairing, and by default the phone sends a recent window of about
// three months. The request travels in the pairing handshake, so it has no
// effect on a device that is already linked, and the phone decides what it
// actually sends.
//
// The zero value keeps whatsmeow's defaults (no full sync requested).
type PairingHistory struct {
	// Full asks the phone for a full history sync instead of the recent
	// window.
	Full bool
	// Days bounds how far back the full sync goes; 0 leaves it to the
	// phone. Requires Full.
	Days uint32
	// SizeMB bounds how many megabytes the phone may put into the full
	// sync; 0 leaves it to the phone. Requires Full.
	SizeMB uint32
}

// Validate reports whether h is a usable configuration.
func (h PairingHistory) Validate() error {
	if !h.Full && (h.Days != 0 || h.SizeMB != 0) {
		return errors.New("history days and size limits need full history enabled")
	}
	if h.Days > MaxPairingHistoryDays {
		return fmt.Errorf("history days %d exceeds the maximum of %d", h.Days, MaxPairingHistoryDays)
	}
	if h.SizeMB > MaxPairingHistorySizeMB {
		return fmt.Errorf("history size %d MB exceeds the maximum of %d MB", h.SizeMB, MaxPairingHistorySizeMB)
	}
	return nil
}

// applyTo writes h into props. A zero h leaves props untouched, so the
// defaults whatsmeow ships stay in force.
func (h PairingHistory) applyTo(props *waCompanionReg.DeviceProps) {
	if !h.Full {
		return
	}
	props.RequireFullSync = proto.Bool(true)
	cfg := props.GetHistorySyncConfig()
	if cfg == nil {
		cfg = &waCompanionReg.DeviceProps_HistorySyncConfig{}
		props.HistorySyncConfig = cfg
	}
	if h.Days > 0 {
		cfg.FullSyncDaysLimit = proto.Uint32(h.Days)
	}
	if h.SizeMB > 0 {
		cfg.FullSyncSizeMbLimit = proto.Uint32(h.SizeMB)
		// The phone may size the sync to the companion's advertised storage,
		// so never advertise less storage than the size asked for.
		if cfg.GetStorageQuotaMb() < h.SizeMB {
			cfg.StorageQuotaMb = proto.Uint32(h.SizeMB)
		}
	}
}

// ApplyPairingHistory validates h and writes it into whatsmeow's
// package-level store.DeviceProps, which whatsmeow serialises into every
// pairing handshake (QR and phone-number code alike). Call it once at
// startup, before the first Connect; it is not safe to call concurrently
// with a connect.
func ApplyPairingHistory(h PairingHistory) error {
	if err := h.Validate(); err != nil {
		return err
	}
	h.applyTo(wmstore.DeviceProps)
	return nil
}

// describe renders h for the startup log.
func (h PairingHistory) describe() string {
	days, size := "phone default", "phone default"
	if h.Days > 0 {
		days = fmt.Sprintf("%d", h.Days)
	}
	if h.SizeMB > 0 {
		size = fmt.Sprintf("%d MB", h.SizeMB)
	}
	return fmt.Sprintf("days: %s, size: %s", days, size)
}

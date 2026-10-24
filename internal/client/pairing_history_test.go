package client

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/util/keys"
)

// restoreDeviceProps snapshots whatsmeow's package-level DeviceProps and
// puts it back when the test ends, so tests that write it stay isolated.
func restoreDeviceProps(t *testing.T) {
	t.Helper()
	saved := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	t.Cleanup(func() { wmstore.DeviceProps = saved })
}

func TestPairingHistoryValidate(t *testing.T) {
	cases := []struct {
		name    string
		h       PairingHistory
		wantErr bool
	}{
		{"zero", PairingHistory{}, false},
		{"full only", PairingHistory{Full: true}, false},
		{"full with limits", PairingHistory{Full: true, Days: 3650, SizeMB: 2048}, false},
		{"at maxima", PairingHistory{Full: true, Days: MaxPairingHistoryDays, SizeMB: MaxPairingHistorySizeMB}, false},
		{"days without full", PairingHistory{Days: 30}, true},
		{"size without full", PairingHistory{SizeMB: 100}, true},
		{"days over max", PairingHistory{Full: true, Days: MaxPairingHistoryDays + 1}, true},
		{"size over max", PairingHistory{Full: true, SizeMB: MaxPairingHistorySizeMB + 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPairingHistoryApplyTo_ZeroKeepsDefaults(t *testing.T) {
	props := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	before := proto.Clone(props)
	PairingHistory{}.applyTo(props)
	if !proto.Equal(props, before) {
		t.Fatalf("zero PairingHistory changed DeviceProps:\nbefore %v\nafter  %v", before, props)
	}
}

func TestPairingHistoryApplyTo_FullOnly(t *testing.T) {
	props := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	wantQuota := props.GetHistorySyncConfig().GetStorageQuotaMb()
	PairingHistory{Full: true}.applyTo(props)

	if !props.GetRequireFullSync() {
		t.Fatal("RequireFullSync not set")
	}
	cfg := props.GetHistorySyncConfig()
	if cfg.FullSyncDaysLimit != nil || cfg.FullSyncSizeMbLimit != nil {
		t.Fatalf("limits set without being asked: days=%v size=%v", cfg.FullSyncDaysLimit, cfg.FullSyncSizeMbLimit)
	}
	if cfg.RecentSyncDaysLimit != nil {
		t.Fatalf("recent window bounded: %d", cfg.GetRecentSyncDaysLimit())
	}
	if got := cfg.GetStorageQuotaMb(); got != wantQuota {
		t.Fatalf("StorageQuotaMb = %d, want unchanged %d", got, wantQuota)
	}
}

func TestPairingHistoryApplyTo_Limits(t *testing.T) {
	props := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	PairingHistory{Full: true, Days: 3650, SizeMB: 20480}.applyTo(props)

	cfg := props.GetHistorySyncConfig()
	if got := cfg.GetFullSyncDaysLimit(); got != 3650 {
		t.Fatalf("FullSyncDaysLimit = %d, want 3650", got)
	}
	if got := cfg.GetFullSyncSizeMbLimit(); got != 20480 {
		t.Fatalf("FullSyncSizeMbLimit = %d, want 20480", got)
	}
	// whatsmeow advertises 10240 MB; a larger size raises the quota with it.
	if got := cfg.GetStorageQuotaMb(); got != 20480 {
		t.Fatalf("StorageQuotaMb = %d, want 20480", got)
	}
	// Unrelated capabilities whatsmeow advertises stay as they were.
	if !cfg.GetInlineInitialPayloadInE2EeMsg() {
		t.Fatal("InlineInitialPayloadInE2EeMsg was cleared")
	}
}

func TestPairingHistoryApplyTo_SmallSizeKeepsLargerQuota(t *testing.T) {
	props := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	props.HistorySyncConfig.StorageQuotaMb = proto.Uint32(10240)
	PairingHistory{Full: true, SizeMB: 512}.applyTo(props)
	if got := props.GetHistorySyncConfig().GetStorageQuotaMb(); got != 10240 {
		t.Fatalf("StorageQuotaMb = %d, want 10240 (never lowered)", got)
	}
}

func TestPairingHistoryApplyTo_NilHistoryConfig(t *testing.T) {
	props := &waCompanionReg.DeviceProps{}
	PairingHistory{Full: true, Days: 30}.applyTo(props)
	if props.GetHistorySyncConfig().GetFullSyncDaysLimit() != 30 {
		t.Fatal("FullSyncDaysLimit not set on a fresh HistorySyncConfig")
	}
}

func TestApplyPairingHistory_RejectsInvalid(t *testing.T) {
	restoreDeviceProps(t)
	before := proto.Clone(wmstore.DeviceProps)
	if err := ApplyPairingHistory(PairingHistory{Days: 30}); err == nil {
		t.Fatal("expected error for days without full history")
	}
	if !proto.Equal(wmstore.DeviceProps, before) {
		t.Fatal("invalid PairingHistory modified DeviceProps")
	}
}

// TestApplyPairingHistory_ReachesRegistrationPayload checks the end of the
// chain without connecting: the DeviceProps whatsmeow marshals into the
// pairing handshake of an unpaired device carry the requested settings.
func TestApplyPairingHistory_ReachesRegistrationPayload(t *testing.T) {
	restoreDeviceProps(t)
	if err := ApplyPairingHistory(PairingHistory{Full: true, Days: 3650, SizeMB: 2048}); err != nil {
		t.Fatalf("ApplyPairingHistory: %v", err)
	}

	identity := keys.NewKeyPair()
	device := &wmstore.Device{
		NoiseKey:     keys.NewKeyPair(),
		IdentityKey:  identity,
		SignedPreKey: identity.CreateSignedPreKey(1),
	}
	payload := device.GetClientPayload()
	raw := payload.GetDevicePairingData().GetDeviceProps()
	if len(raw) == 0 {
		t.Fatal("registration payload carries no DeviceProps")
	}
	var sent waCompanionReg.DeviceProps
	if err := proto.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("unmarshal DeviceProps: %v", err)
	}
	if !sent.GetRequireFullSync() {
		t.Fatal("handshake RequireFullSync = false, want true")
	}
	cfg := sent.GetHistorySyncConfig()
	if cfg.GetFullSyncDaysLimit() != 3650 || cfg.GetFullSyncSizeMbLimit() != 2048 {
		t.Fatalf("handshake limits = %d days / %d MB, want 3650 / 2048",
			cfg.GetFullSyncDaysLimit(), cfg.GetFullSyncSizeMbLimit())
	}
}

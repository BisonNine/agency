package events

import "testing"

func TestFormatMessage(t *testing.T) {
	tests := []struct {
		name string
		ev   DeviceEvent
		want string
	}{
		{
			name: "connected with details",
			ev:   DeviceEvent{Action: ActionAdd, Kind: KindUSB, Vendor: "Acme", Product: "Stick", Capabilities: "Mass Storage", Serial: "S1"},
			want: "🔌 Device Connected\n\nType: usb\nDevice: Acme Stick\nCapabilities: Mass Storage\nSerial: S1\n",
		},
		{
			name: "disconnected bare",
			ev:   DeviceEvent{Action: ActionRemove, Kind: KindBluetooth, Vendor: "Acme", Product: "Buds"},
			want: "🔌 Device Disconnected\n\nType: bluetooth\nDevice: Acme Buds\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ev.FormatMessage(); got != tt.want {
				t.Errorf("FormatMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

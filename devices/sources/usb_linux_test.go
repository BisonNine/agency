//go:build linux

package sources

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/devices/events"
)

func TestParseUSBEvent(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		p := map[string]string{"SUBSYSTEM": "usb", "DEVTYPE": "usb_device"}
		maps.Copy(p, extra)
		return p
	}
	tests := []struct {
		name   string
		action string
		props  map[string]string
		want   *events.DeviceEvent // nil: ignored
	}{
		{"other subsystem", "add", map[string]string{"SUBSYSTEM": "block"}, nil},
		{"interface", "add", base(map[string]string{"DEVTYPE": "usb_interface"}), nil},
		{"other devtype", "add", base(map[string]string{"DEVTYPE": "usb_hub"}), nil},
		{"change action", "change", base(nil), nil},
		{
			"named device", "add",
			base(map[string]string{"ID_VENDOR": "Acme", "ID_MODEL": "Stick", "ID_SERIAL_SHORT": "S1", "BUSNUM": "001", "DEVNUM": "004", "ID_USB_CLASS": "08"}),
			&events.DeviceEvent{Action: events.ActionAdd, Kind: events.KindUSB, Vendor: "Acme", Product: "Stick", Serial: "S1", DeviceID: "001:004", Capabilities: "Mass Storage (USB Flash Drive/Hard Disk)"},
		},
		{
			"ids only, no devtype", "remove",
			map[string]string{"SUBSYSTEM": "usb", "ID_VENDOR_ID": "1234", "ID_MODEL_ID": "abcd", "DEVPATH": "/devices/usb1/1-2", "BUSNUM": "001", "ID_USB_CLASS": "FF"},
			&events.DeviceEvent{Action: events.ActionRemove, Kind: events.KindUSB, Vendor: "1234", Product: "abcd", DeviceID: "/devices/usb1/1-2", Capabilities: "Vendor Specific"},
		},
		{
			"nothing known", "add", base(map[string]string{"ID_USB_CLASS": "77"}),
			&events.DeviceEvent{Action: events.ActionAdd, Kind: events.KindUSB, Vendor: "Unknown Vendor", Product: "Unknown Device", Capabilities: "USB Device"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUSBEvent(tt.action, tt.props)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if got.Raw["SUBSYSTEM"] != "usb" {
				t.Errorf("Raw = %v, want the udev properties", got.Raw)
			}
			got.Raw = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

// fakeUdevadm puts a udevadm script on PATH that prints body and then runs
// tail (empty: exits).
func fakeUdevadm(t *testing.T, body, tail string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "EOF\n" + tail + "\n"
	if err := os.WriteFile(filepath.Join(dir, "udevadm"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
}

const udevOutput = `KERNEL[1.0] add /devices/usb1/1-2 (usb)
ACTION=add
SUBSYSTEM=usb
DEVTYPE=usb_device

UDEV  [1.1] add /devices/usb1/1-2 (usb)
ACTION=add
SUBSYSTEM=usb
DEVTYPE=usb_device
ID_VENDOR=Acme
ID_MODEL=Stick

UDEV  [1.2] add /devices/usb1/1-2:1.0 (usb)
ACTION=add
SUBSYSTEM=usb
DEVTYPE=usb_interface

UDEV  [2.0] remove /devices/usb1/1-2 (usb)
ACTION=remove
SUBSYSTEM=usb
DEVTYPE=usb_device
ID_VENDOR=Acme
ID_MODEL=Stick

`

func collect(t *testing.T, ch <-chan *events.DeviceEvent) []*events.DeviceEvent {
	t.Helper()
	var out []*events.DeviceEvent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("event channel did not close")
		}
	}
}

// Only complete UDEV blocks for devices become events; KERNEL blocks and
// interfaces are skipped, and the channel closes when udevadm ends.
func TestUSBMonitor_StreamsUdevEvents(t *testing.T) {
	fakeUdevadm(t, udevOutput, "")
	m := NewUSBMonitor()
	if m.Kind() != events.KindUSB {
		t.Errorf("Kind = %s", m.Kind())
	}
	ch, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Action != events.ActionAdd || got[0].Vendor != "Acme" || got[1].Action != events.ActionRemove {
		t.Errorf("events = %+v, %+v", *got[0], *got[1])
	}
	if err := m.Stop(); err != nil {
		t.Errorf("Stop after exit = %v", err)
	}
}

// Stop ends a running udevadm and the channel closes.
func TestUSBMonitor_StopKills(t *testing.T) {
	fakeUdevadm(t, "", "exec sleep 1000")
	m := NewUSBMonitor()
	ch, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if got := collect(t, ch); len(got) != 0 {
		t.Errorf("got %d events", len(got))
	}
	if err := m.Stop(); err != nil {
		t.Errorf("second Stop = %v", err)
	}
}

func TestUSBMonitor_NoUdevadm(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewUSBMonitor().Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is udevadm installed?") {
		t.Fatalf("err = %v", err)
	}
}

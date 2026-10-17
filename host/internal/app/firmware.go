package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/since2006/go-dock/host/internal/device"
	"github.com/since2006/go-dock/host/internal/update"
)

// The screen firmware goes with go-dock: each release carries both, built
// from the same tag, and go-dock offers the device the firmware of its own
// version. GO_DOCK_FIRMWARE_FILE points at a local app image instead, to
// try a firmware build on the device before releasing it.
const firmwareFileEnv = "GO_DOCK_FIRMWARE_FILE"

// deviceStatus is the device link plus the firmware this go-dock offers.
type deviceStatus struct {
	device.Status
	// Target is the firmware version that goes with this go-dock; empty for
	// a development build.
	Target string `json:"target,omitempty"`
	// Upgradable: the device runs other firmware and can be updated over USB.
	Upgradable bool `json:"upgradable,omitempty"`
	// Manual: the device runs older firmware that cannot be updated over
	// USB yet; it needs flashing by hand once.
	Manual bool `json:"manual,omitempty"`
}

func (a *App) deviceStatus() deviceStatus {
	st := deviceStatus{Status: a.device.Status()}
	file := os.Getenv(firmwareFileEnv)
	switch {
	case file != "":
		v, err := imageFileVersion(file)
		if err != nil {
			st.Error = fmt.Sprintf("%s: %v", firmwareFileEnv, err)
			return st
		}
		st.Target = v
	case !a.updater.Status().Dev:
		st.Target = a.version
	}
	if st.Target == "" || st.Firmware == "" || st.Firmware == st.Target {
		return st
	}
	if file != "" || update.Newer(st.Target, st.Firmware) || st.Incompatible {
		st.Upgradable, st.Manual = st.OTA, !st.OTA
	}
	return st
}

// imageFileVersion reads the version from an app image's header.
func imageFileVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 256)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return device.CheckImage(head[:n])
}

// ApplyFirmware updates the device to the firmware that goes with this
// go-dock, in the background; progress shows up in DeviceStatus.
func (a *App) ApplyFirmware() error {
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	st := a.deviceStatus()
	switch {
	case a.hostUpdating.Load():
		return errors.New("go-dock 正在升级，请稍后再升级固件")
	case st.Flash.Busy():
		return errors.New("固件正在升级")
	case st.Manual:
		return errors.New("设备当前的固件不支持在线升级，请先用 USB 刷一次")
	case !st.Upgradable:
		return errors.New("没有可升级的固件")
	}
	target, file := st.Target, os.Getenv(firmwareFileEnv)
	return a.device.StartFlash(a.ctx, func(ctx context.Context) ([]byte, error) {
		if file != "" {
			return os.ReadFile(file)
		}
		img, err := a.updater.Firmware(ctx, target)
		if err != nil {
			return nil, err
		}
		if v, err := device.CheckImage(img); err == nil && v != target {
			return nil, fmt.Errorf("版本 %s 的发布里固件版本是 %s", target, v)
		}
		return img, nil
	})
}

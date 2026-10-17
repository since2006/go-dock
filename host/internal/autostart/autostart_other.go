//go:build !darwin && !linux && !windows

package autostart

import "errors"

var errUnsupported = errors.New("当前系统不支持开机自启")

func get(string) (Status, error) { return Status{}, nil }
func enable(string) error        { return errUnsupported }
func disable() error             { return errUnsupported }

//go:build !darwin && !linux && !windows

package sysmon

import "context"

type other struct{}

func newPlatform() platform { return other{} }

func (other) fans(context.Context) []Fan      { return nil }
func (other) gpu(context.Context) GPU         { return NoGPU }
func (other) memory(context.Context, *Memory) {}

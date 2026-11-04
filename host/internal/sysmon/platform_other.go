//go:build !darwin && !linux && !windows

package sysmon

import "context"

type other struct{}

func newPlatform() platform { return other{} }

func (other) fans(context.Context) []float64  { return nil }
func (other) gpu(context.Context) float64     { return -1 }
func (other) temp(context.Context) float64    { return 0 }
func (other) memory(context.Context, *Memory) {}

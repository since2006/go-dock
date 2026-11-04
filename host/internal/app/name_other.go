//go:build !darwin

package app

// computerName is empty where the hostname is the computer's name.
func computerName() string { return "" }

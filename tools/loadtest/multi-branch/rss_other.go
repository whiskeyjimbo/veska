//go:build !linux

package main

func currentRSSBytes() (int64, error) { return 0, nil }

//go:build !linux && multi_branch_bench

package main

func currentRSSBytes() (int64, error) { return 0, nil }

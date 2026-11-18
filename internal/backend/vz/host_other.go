//go:build !darwin

package vz

func translated() bool           { return false }
func checkAPFS(dir string) error { return nil }

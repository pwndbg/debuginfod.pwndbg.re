//go:build !linux

package main

import "errors"

type mountKind string

const (
	mountFileBacked mountKind = "file-backed"
	mountLoop       mountKind = "loop"
)

// This service only ever runs on Linux; the stub exists so the package still
// builds on a dev machine, where the tests that need a mount skip themselves.
var errMountUnsupported = errors.New("erofs mounting is only supported on linux")

func mountErofs(image, target string) (mountKind, error) { return "", errMountUnsupported }
func unmountErofs(target string) error                   { return errMountUnsupported }

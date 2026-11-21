//go:build !darwin && !linux

package storage

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("Roast's ChatGPT sign-in is available on macOS and Linux")

func CheckSupported() error { return errUnsupported }

func readPrivateFile(string) ([]byte, error) { return nil, errUnsupported }

func WriteJSONContext(context.Context, string, any) error { return errUnsupported }

func CleanupTemps(string) error { return errUnsupported }

func WithLock(context.Context, string, func() error) error { return errUnsupported }

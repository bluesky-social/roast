//go:build !darwin && !linux

package storage

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("Roast's ChatGPT sign-in is available on macOS and Linux")

func CheckSupported() error { return errUnsupported }

func ReadJSON(string, any) error { return errUnsupported }

func WriteJSON(string, any) error { return errUnsupported }

func WriteJSONContext(context.Context, string, any) error { return errUnsupported }

func CleanupTemps(string) error { return errUnsupported }

func WithLock(context.Context, string, func() error) error { return errUnsupported }

//go:build !darwin && !linux

package storage

import "errors"

func Open() (Store, error) {
	return nil, errors.New("ChatGPT sign-in is available on macOS and Linux")
}

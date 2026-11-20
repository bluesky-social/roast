package storage

import "errors"

// ErrPublished marks a write that became visible before its final directory sync failed.
var ErrPublished = errors.New("ChatGPT credentials were published")

package oauth

import "errors"

const APIBaseURL = "https://api.openai.com/v1"

var ErrAuth = errors.New("ChatGPT authentication required")

package chatgpt

import "github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"

// APIBaseURL is the shared endpoint for ChatGPT OAuth and inference.
const APIBaseURL = oauth.APIBaseURL

var ErrAuth = oauth.ErrAuth

package osclient

var (
	RefreshDelay = refreshDelay
	TokensURL    = tokensURL
)

var CheckRedirect = checkRedirect

// SetMaxPages lowers the page limit for a test and returns a function that restores it.
func SetMaxPages(n int) func() {
	old := maxPages
	maxPages = n
	return func() { maxPages = old }
}

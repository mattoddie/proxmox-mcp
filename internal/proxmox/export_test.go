package proxmox

import "time"

// Test hooks for the external test package.

// LoginRetryAt is when a login may next be attempted after refused credentials.
func (c *Client) LoginRetryAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginRetryAt
}

// SetLoginRetryAt moves the login backoff, e.g. to end it now.
func (c *Client) SetLoginRetryAt(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loginRetryAt = t
}

// SetMaxResponseBytes lowers the response size limit and returns a function restoring it.
func SetMaxResponseBytes(n int) func() {
	old := maxResponseBytes
	maxResponseBytes = n
	return func() { maxResponseBytes = old }
}

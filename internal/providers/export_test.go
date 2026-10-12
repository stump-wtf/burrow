package providers

import "time"

// Test seams for the menu cache: gateway calls run behind the requests that
// start them, so a test waits for them to land instead of sleeping, and
// shrinks the two waits a hung gateway would otherwise cost it.

// SetTimeouts replaces the bound on one gateway call and the longest a
// request waits for rows that have no answer yet.
func (r *Runtime) SetTimeouts(fetch, firstWait time.Duration) {
	r.http.Timeout, r.coldWait = fetch, firstWait
}

// WaitRefreshes blocks until no gateway call is in flight.
func (r *Runtime) WaitRefreshes() { r.calls.Wait() }

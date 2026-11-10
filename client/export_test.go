package client

import "time"

// Pongs reports how many of the liveness check's pings got their pong.
func Pongs(c *Client) int64 { return c.pongs.Load() }

// LiveChecks reports how many sessions were dialed with the check on.
func LiveChecks(c *Client) int64 { return c.liveChecks.Load() }

// LivenessTimeoutOf reports the timeout the client runs with.
func LivenessTimeoutOf(c *Client) time.Duration { return c.opts.LivenessTimeout }

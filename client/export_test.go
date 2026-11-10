package client

// Pongs reports how many of the liveness check's pings got their pong.
func Pongs(c *Client) int64 { return c.pongs.Load() }

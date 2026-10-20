package server

import "time"

// SetCloseHandshakeLimit shortens how long a close the server starts may take
// before the server cuts the transport, so a test of that bound runs fast.
// Call it before the server's first connection.
func SetCloseHandshakeLimit(s *Server, d time.Duration) { s.closeLimit = d }

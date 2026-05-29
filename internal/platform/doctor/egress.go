package doctor

import (
	"net"
	"time"
)

// SocketStatus holds the probe result for a single Unix-domain socket.
type SocketStatus struct {
	Path string `json:"path"`
	// Status is one of: "reachable", "missing".
	Status string `json:"status"`
}

// EgressReport holds the results of probing all configured outbound sockets.
type EgressReport struct {
	Sockets []SocketStatus `json:"sockets"`
}

// CheckEgress attempts a 200ms dial to each Unix-domain socket path in sockPaths
// and reports which are reachable vs. missing.  It never returns a non-nil error —
// connectivity failures are reflected in each SocketStatus.Status field.
func CheckEgress(sockPaths []string) (EgressReport, error) {
	statuses := make([]SocketStatus, 0, len(sockPaths))
	for _, path := range sockPaths {
		ss := SocketStatus{Path: path}
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err != nil {
			ss.Status = "missing"
		} else {
			conn.Close()
			ss.Status = "reachable"
		}
		statuses = append(statuses, ss)
	}
	return EgressReport{Sockets: statuses}, nil
}

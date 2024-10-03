package utils

const (
	SG_HB   byte = iota // for heartbeat
	SG_Chan             // for channel, req a new conn
	SG_Ping             // for ping
)

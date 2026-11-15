// relayd is a loopback-only WebRTC relay node used to validate forwarding
// behavior offline: one ingress PeerConnection, N egress PeerConnections.
package main

import (
	"fmt"
	"os"
)

// Exit codes.
const (
	exitOK     = 0
	exitSignal = 2 // signaling failure
	exitICE    = 3 // ICE / connection failure
	exitStats  = 4 // statistics inconsistency
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: relayd <command> [flags]

commands:
  signal    run the signaling server (optionally self-check rejections)
  forward   run the relay node (ingress -> N egress legs)
  selftest  deterministic end-to-end self test, writes a JSON report
  report    read back and validate a JSON report

exit codes: 0 ok, 2 signaling, 3 ICE/connection, 4 statistics mismatch`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitSignal)
	}
	var code int
	switch os.Args[1] {
	case "signal":
		code = cmdSignal(os.Args[2:])
	case "forward":
		code = cmdForward(os.Args[2:])
	case "selftest":
		code = cmdSelftest(os.Args[2:])
	case "report":
		code = cmdReport(os.Args[2:])
	default:
		usage()
		code = exitSignal
	}
	os.Exit(code)
}
